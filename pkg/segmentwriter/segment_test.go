package segmentwriter

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gprofile "github.com/google/pprof/profile"
	"github.com/grafana/dskit/flagext"
	prommodel "github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	profilev1 "github.com/grafana/pyroscope/api/gen/proto/go/google/v1"
	ingesterv1 "github.com/grafana/pyroscope/api/gen/proto/go/ingester/v1"
	"github.com/grafana/pyroscope/api/gen/proto/go/ingester/v1/ingesterv1connect"
	metastorev1 "github.com/grafana/pyroscope/api/gen/proto/go/metastore/v1"
	typesv1 "github.com/grafana/pyroscope/api/gen/proto/go/types/v1"
	"github.com/grafana/pyroscope/pkg/block"
	"github.com/grafana/pyroscope/pkg/block/metadata"
	"github.com/grafana/pyroscope/pkg/metastore"
	"github.com/grafana/pyroscope/pkg/metastore/index/dlq"
	metastoretest "github.com/grafana/pyroscope/pkg/metastore/test"
	"github.com/grafana/pyroscope/pkg/model"
	"github.com/grafana/pyroscope/pkg/objstore/providers/filesystem"
	"github.com/grafana/pyroscope/pkg/objstore/providers/memory"
	"github.com/grafana/pyroscope/pkg/og/convert/pprof/bench"
	"github.com/grafana/pyroscope/pkg/phlaredb"
	testutil3 "github.com/grafana/pyroscope/pkg/phlaredb/block/testutil"
	pprofth "github.com/grafana/pyroscope/pkg/pprof/testhelper"
	"github.com/grafana/pyroscope/pkg/segmentwriter/memdb"
	memdbtest "github.com/grafana/pyroscope/pkg/segmentwriter/memdb/testutil"
	"github.com/grafana/pyroscope/pkg/test"
	"github.com/grafana/pyroscope/pkg/test/mocks/mockdlq"
	"github.com/grafana/pyroscope/pkg/test/mocks/mockmetastorev1"
	"github.com/grafana/pyroscope/pkg/test/mocks/mockobjstore"
	"github.com/grafana/pyroscope/pkg/validation"
)

func TestSegmentIngest(t *testing.T) {
	td := [][]inputChunk{
		staticTestData(),
		testDataGenerator{
			seed:     239,
			chunks:   3,
			profiles: 256,
			shards:   4,
			tenants:  3,
			services: 5,
		}.generate(),
		//testDataGenerator{
		//	seed:     time.Now().UnixNano(),
		//	chunks:   3,
		//	profiles: 4096,
		//	shards:   8,
		//	tenants:  12,
		//	services: 16,
		//}.generate(),
	}
	for i, chunks := range td {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			t.Run("ingestWithMetastoreAvailable", func(t *testing.T) {
				ingestWithMetastoreAvailable(t, chunks)
			})
			t.Run("ingestWithDLQ", func(t *testing.T) {
				ingestWithDLQ(t, chunks)
			})
		})
	}
}

func ingestWithMetastoreAvailable(t *testing.T, chunks []inputChunk) {
	sw := newTestSegmentWriter(t, defaultTestConfig())
	defer sw.stop()
	blocks := make(chan *metastorev1.BlockMeta, 128)

	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			blocks <- args.Get(1).(*metastorev1.AddBlockRequest).Block
		}).Return(new(metastorev1.AddBlockResponse), nil)
	for _, chunk := range chunks {
		chunkBlocks := make([]*metastorev1.BlockMeta, 0, len(chunk))
		waiterSet := sw.ingestChunk(t, chunk, false)
		for range waiterSet {
			meta := <-blocks
			chunkBlocks = append(chunkBlocks, meta)
		}
		inputs := groupInputs(t, chunk)
		clients := sw.createBlocksFromMetas(chunkBlocks)
		sw.queryInputs(clients, inputs)
	}
}

func ingestWithDLQ(t *testing.T, chunks []inputChunk) {
	sw := newTestSegmentWriter(t, defaultTestConfig())
	defer sw.stop()
	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("metastore unavailable"))
	ingestedChunks := make([]inputChunk, 0, len(chunks))
	for chunkIndex, chunk := range chunks {
		t.Logf("ingesting chunk %d", chunkIndex)
		_ = sw.ingestChunk(t, chunk, false)
		ingestedChunks = append(ingestedChunks, chunk)
		allBlocks := sw.getMetadataDLQ()
		clients := sw.createBlocksFromMetas(allBlocks)
		inputs := groupInputs(t, ingestedChunks...)
		t.Logf("querying chunk %d", chunkIndex)
		sw.queryInputs(clients, inputs)
	}
}

func TestIngestWait(t *testing.T) {
	sw := newTestSegmentWriter(t, defaultTestConfig())

	defer sw.stop()
	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		time.Sleep(1 * time.Second)
	}).Return(new(metastorev1.AddBlockResponse), nil)

	t1 := time.Now()
	awaiter := sw.ingest(0, func(head segmentIngest) {
		p := cpuProfile(42, 480, "svc1", "foo", "bar")
		head.ingest("t1", p.Profile, p.UUID, p.Labels, p.Annotations)
	})
	err := awaiter.waitFlushed(context.Background())
	require.NoError(t, err)
	since := time.Since(t1)
	require.True(t, since > 1*time.Second)
}

func TestBusyIngestLoop(t *testing.T) {

	sw := newTestSegmentWriter(t, defaultTestConfig())
	defer sw.stop()

	writeCtx, writeCancel := context.WithCancel(context.Background())
	readCtx, readCancel := context.WithCancel(context.Background())
	metaChan := make(chan *metastorev1.BlockMeta)

	var cnt atomic.Int32
	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			metaChan <- args.Get(1).(*metastorev1.AddBlockRequest).Block
			if cnt.Add(1) == 3 {
				writeCancel()
			}
		}).Return(new(metastorev1.AddBlockResponse), nil)
	metas := make([]*metastorev1.BlockMeta, 0)
	readG := sync.WaitGroup{}
	readG.Add(1)
	go func() {
		defer readG.Done()
		for {
			select {
			case <-readCtx.Done():
				return
			case meta := <-metaChan:
				metas = append(metas, meta)
			}
		}
	}()
	writeG := sync.WaitGroup{}
	allProfiles := make([]*pprofth.ProfileBuilder, 0)
	m := new(sync.Mutex)
	nWorkers := 5
	for i := 0; i < nWorkers; i++ {
		workerno := i
		writeG.Add(1)
		go func() {
			defer writeG.Done()
			awaiters := make([]segmentWaitFlushed, 0)
			profiles := make([]*pprofth.ProfileBuilder, 0)
			defer func() {
				require.NotEmpty(t, profiles)
				require.NotEmpty(t, awaiters)
				for _, awaiter := range awaiters {
					err := awaiter.waitFlushed(context.Background())
					require.NoError(t, err)
				}
				m.Lock()
				allProfiles = append(allProfiles, profiles...)
				m.Unlock()
			}()
			for {
				select {
				case <-writeCtx.Done():
					return
				default:
					ts := workerno*1000000000 + len(profiles)
					awaiter := sw.ingest(1, func(head segmentIngest) {
						p := cpuProfile(42, ts, "svc1", "foo", "bar")
						head.ingest("t1", p.CloneVT(), p.UUID, p.Labels, p.Annotations)
						profiles = append(profiles, p)
					})
					awaiters = append(awaiters, awaiter)
				}
			}
		}()
	}
	writeG.Wait()

	readCancel()
	readG.Wait()
	assert.True(t, len(metas) >= 3)

	chunk := make(inputChunk, 0)
	for _, p := range allProfiles {
		chunk = append(chunk, input{shard: 1, tenant: "t1", profile: p})
	}
	inputs := groupInputs(t, chunk)
	clients := sw.createBlocksFromMetas(metas)
	sw.queryInputs(clients, inputs)
}

func TestDLQFail(t *testing.T) {
	l := test.NewTestingLogger(t)
	bucket := mockobjstore.NewMockBucket(t)
	bucket.On("Upload", mock.Anything, mock.MatchedBy(func(name string) bool {
		return isSegmentPath(name)
	}), mock.Anything).Return(nil)
	bucket.On("Upload", mock.Anything, mock.MatchedBy(func(name string) bool {
		return isDLQPath(name)
	}), mock.Anything).Return(fmt.Errorf("mock upload DLQ error"))
	client := mockmetastorev1.NewMockIndexServiceClient(t)
	client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("mock add block error"))

	res := newSegmentWriter(
		l,
		newSegmentMetrics(nil),
		memdb.NewHeadMetricsWithPrefix(nil, ""),
		defaultTestConfig(),
		validation.MockDefaultOverrides(),
		bucket,
		client,
	)
	defer res.stop()
	ts := 420
	ing := func(head segmentIngest) {
		ts += 420
		p := cpuProfile(42, ts, "svc1", "foo", "bar")
		head.ingest("t1", p.Profile, p.UUID, p.Labels, p.Annotations)
	}

	awaiter1 := res.ingest(0, ing)
	awaiter2 := res.ingest(0, ing)

	err1 := awaiter1.waitFlushed(context.Background())
	require.Error(t, err1)

	err2 := awaiter1.waitFlushed(context.Background())
	require.Error(t, err2)

	err3 := awaiter2.waitFlushed(context.Background())
	require.Error(t, err3)

	require.Equal(t, err1, err2)
	require.Equal(t, err1, err3)
}

func TestDatasetMinMaxTime(t *testing.T) {
	l := test.NewTestingLogger(t)
	bucket := memory.NewInMemBucket()
	metas := make(chan *metastorev1.BlockMeta)
	client := mockmetastorev1.NewMockIndexServiceClient(t)
	client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			meta := args.Get(1).(*metastorev1.AddBlockRequest).Block
			metas <- meta
		}).Return(new(metastorev1.AddBlockResponse), nil)
	res := newSegmentWriter(
		l,
		newSegmentMetrics(nil),
		memdb.NewHeadMetricsWithPrefix(nil, ""),
		defaultTestConfig(),
		validation.MockDefaultOverrides(),
		bucket,
		client,
	)
	data := []input{
		{shard: 1, tenant: "tb", profile: cpuProfile(42, 239, "svc1", "foo", "bar")},
		{shard: 1, tenant: "tb", profile: cpuProfile(13, 420, "svc1", "qwe", "foo", "bar")},
		{shard: 1, tenant: "tb", profile: cpuProfile(13, 420, "svc2", "qwe", "foo", "bar")},
		{shard: 1, tenant: "tb", profile: cpuProfile(13, 421, "svc2", "qwe", "foo", "bar")},
		{shard: 1, tenant: "ta", profile: cpuProfile(13, 10, "svc1", "vbn", "foo", "bar")},
		{shard: 1, tenant: "ta", profile: cpuProfile(13, 1337, "svc1", "vbn", "foo", "bar")},
	}
	_ = res.ingest(1, func(head segmentIngest) {
		for _, p := range data {
			head.ingest(p.tenant, p.profile.Profile, p.profile.UUID, p.profile.Labels, p.profile.Annotations)
		}
	})
	defer res.stop()

	block := <-metas

	expected := [][2]int{
		{10, 1337},
		{239, 420},
		{420, 421},
	}

	require.Equal(t, len(expected), len(block.Datasets))
	for i, ds := range block.Datasets {
		assert.Equalf(t, expected[i][0], int(ds.MinTime), "idx %d", i)
		assert.Equalf(t, expected[i][1], int(ds.MaxTime), "idx %d", i)
	}
	assert.Equal(t, int64(10), block.MinTime)
	assert.Equal(t, int64(1337), block.MaxTime)
}

func TestQueryMultipleSeriesSingleTenant(t *testing.T) {
	metas := make(chan *metastorev1.BlockMeta, 1)

	sw := newTestSegmentWriter(t, defaultTestConfig())
	defer sw.stop()
	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			metas <- args.Get(1).(*metastorev1.AddBlockRequest).Block
		}).Return(new(metastorev1.AddBlockResponse), nil)

	data := inputChunk([]input{
		{shard: 1, tenant: "tb", profile: cpuProfile(42, 239, "svc1", "kek", "foo", "bar")},
		{shard: 1, tenant: "tb", profile: cpuProfile(13, 420, "svc1", "qwe1", "foo", "bar")},
		{shard: 1, tenant: "tb", profile: cpuProfile(17, 420, "svc2", "qwe3", "foo", "bar")},
		{shard: 1, tenant: "tb", profile: cpuProfile(13, 421, "svc2", "qwe", "foo", "bar")},
		{shard: 1, tenant: "ta", profile: cpuProfile(13, 10, "svc1", "vbn", "foo", "bar")},
		{shard: 1, tenant: "ta", profile: cpuProfile(13, 1337, "svc1", "vbn", "foo", "bar")},
	})
	sw.ingestChunk(t, data, false)
	block := <-metas

	clients := sw.createBlocksFromMetas([]*metastorev1.BlockMeta{block})
	defer func() {
		for _, tc := range clients {
			tc.f()
		}
	}()

	client := clients["tb"]
	actualMerged := sw.query(client, &ingesterv1.SelectProfilesRequest{
		LabelSelector: "{service_name=~\"svc[12]\"}",
		Type:          mustParseProfileSelector(t, "process_cpu:cpu:nanoseconds:cpu:nanoseconds"),
		Start:         239,
		End:           420,
	})
	expectedMerged := mergeProfiles(t, []*profilev1.Profile{
		data[0].profile.Profile,
		data[1].profile.Profile,
		data[2].profile.Profile,
	})
	actualCollapsed := bench.StackCollapseProto(actualMerged, 0, 1)
	expectedCollapsed := bench.StackCollapseProto(expectedMerged, 0, 1)
	require.Equal(t, expectedCollapsed, actualCollapsed)
}

func TestDLQRecoveryMock(t *testing.T) {
	chunk := inputChunk([]input{
		{shard: 1, tenant: "tb", profile: cpuProfile(42, 239, "svc1", "kek", "foo", "bar")},
	})

	sw := newTestSegmentWriter(t, defaultTestConfig())
	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("mock metastore unavailable"))

	_ = sw.ingestChunk(t, chunk, false)
	allBlocks := sw.getMetadataDLQ()
	assert.Len(t, allBlocks, 1)

	recoveredMetas := make(chan *metastorev1.BlockMeta, 1)
	srv := mockdlq.NewMockMetastore(t)
	srv.On("AddRecoveredBlock", mock.Anything, mock.Anything).
		Once().
		Run(func(args mock.Arguments) {
			meta := args.Get(1).(*metastorev1.AddBlockRequest).Block
			recoveredMetas <- meta
		}).
		Return(&metastorev1.AddBlockResponse{}, nil)
	recovery := dlq.NewRecovery(test.NewTestingLogger(t), dlq.Config{
		CheckInterval: 100 * time.Millisecond,
	}, srv, sw.bucket)
	recovery.Start()
	defer recovery.Stop()

	meta := <-recoveredMetas
	assert.Equal(t, allBlocks[0].Id, meta.Id)

	clients := sw.createBlocksFromMetas(allBlocks)
	inputs := groupInputs(t, chunk)
	sw.queryInputs(clients, inputs)
}

func TestDLQRecovery(t *testing.T) {
	const tenant = "tb"
	var ts = time.Now().UnixMilli()
	chunk := inputChunk([]input{
		{shard: 1, tenant: tenant, profile: cpuProfile(42, int(ts), "svc1", "kek", "foo", "bar")},
	})

	sw := newTestSegmentWriter(t, defaultTestConfig())
	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("mock metastore unavailable"))

	_ = sw.ingestChunk(t, chunk, false)

	cfg := new(metastore.Config)
	flagext.DefaultValues(cfg)
	cfg.Index.Recovery.CheckInterval = 100 * time.Millisecond
	m := metastoretest.NewMetastoreSet(t, cfg, 3, sw.bucket)
	defer m.Close()

	queryBlock := func() *metastorev1.BlockMeta {
		res, err := m.Client.QueryMetadata(context.Background(), &metastorev1.QueryMetadataRequest{
			TenantId:  []string{tenant},
			StartTime: ts - 1,
			EndTime:   ts + 1,
			Query:     "{service_name=~\"svc1\"}",
		})
		if err != nil {
			return nil
		}
		if len(res.Blocks) == 1 {
			return res.Blocks[0]
		}
		return nil
	}
	require.Eventually(t, func() bool {
		return queryBlock() != nil
	}, 10*time.Second, 100*time.Millisecond)

	block := queryBlock()
	require.NotNil(t, block)

	clients := sw.createBlocksFromMetas([]*metastorev1.BlockMeta{block})
	inputs := groupInputs(t, chunk)
	sw.queryInputs(clients, inputs)
}

func TestUnsymbolizedLabelIsSet(t *testing.T) {
	sw := newTestSegmentWriter(t, defaultTestConfig())
	defer sw.stop()
	blocks := make(chan *metastorev1.BlockMeta, 1)

	sw.client.On("AddBlock", mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			blocks <- args.Get(1).(*metastorev1.AddBlockRequest).Block
		}).Return(new(metastorev1.AddBlockResponse), nil)

	p := pprofth.NewProfileBuilder(time.Now().UnixNano()).
		CPUProfile().
		WithLabels(model.LabelNameServiceName, "svc1")

	p.Mapping = []*profilev1.Mapping{
		{Id: 1, HasFunctions: false},
	}

	loc := &profilev1.Location{
		Id:        1,
		MappingId: 1,
		Line:      nil,
	}
	p.Location = append(p.Location, loc)

	keyIdx := int64(len(p.StringTable))
	p.StringTable = append(p.StringTable, "foo")
	valIdx := int64(len(p.StringTable))
	p.StringTable = append(p.StringTable, "bar")

	sample1 := &profilev1.Sample{
		LocationId: []uint64{1},
		Value:      []int64{1},
		Label: []*profilev1.Label{
			{Key: keyIdx, Str: valIdx},
		},
	}
	p.Sample = append(p.Sample, sample1)

	sample2 := &profilev1.Sample{
		LocationId: []uint64{1},
		Value:      []int64{2},
		Label:      nil,
	}
	p.Sample = append(p.Sample, sample2)

	chunk := inputChunk{
		{shard: 1, tenant: "t1", profile: p},
	}
	_ = sw.ingestChunk(t, chunk, false)
	block := <-blocks

	require.True(t, hasUnsymbolizedLabel(t, block))
}

type sw struct {
	*segmentsWriter
	bucket  *memory.InMemBucket
	client  *mockmetastorev1.MockIndexServiceClient
	t       *testing.T
	queryNo int
}

func newTestSegmentWriter(t *testing.T, cfg Config) sw {
	l := test.NewTestingLogger(t)
	bucket := memory.NewInMemBucket()
	client := mockmetastorev1.NewMockIndexServiceClient(t)
	res := newSegmentWriter(
		l,
		newSegmentMetrics(nil),
		memdb.NewHeadMetricsWithPrefix(nil, ""),
		cfg,
		validation.MockDefaultOverrides(),
		bucket,
		client,
	)
	return sw{
		t:              t,
		segmentsWriter: res,
		bucket:         bucket,
		client:         client,
	}
}

func defaultTestConfig() Config {
	return Config{
		SegmentDuration:       100 * time.Millisecond,
		UploadTimeout:         time.Second,
		MetadataUpdateTimeout: time.Second,
		MetadataDLQEnabled:    true,
	}
}

func (sw *sw) createBlocksFromMetas(blocks []*metastorev1.BlockMeta) tenantClients {
	dir := sw.t.TempDir()
	for _, meta := range blocks {
		blobReader, err := sw.bucket.Get(context.Background(), block.ObjectPath(meta))
		require.NoError(sw.t, err)
		blob, err := io.ReadAll(blobReader)
		require.NoError(sw.t, err)

		for _, ds := range meta.Datasets {
			tenant := meta.StringTable[ds.Tenant]
			profiles := blob[ds.TableOfContents[0]:ds.TableOfContents[1]]
			tsdb := blob[ds.TableOfContents[1]:ds.TableOfContents[2]]
			symbols := blob[ds.TableOfContents[2] : ds.TableOfContents[0]+ds.Size]
			testutil3.CreateBlockFromMemory(sw.t,
				filepath.Join(dir, tenant),
				prommodel.TimeFromUnixNano(ds.MinTime*1e6), //todo  do not use 1e6, add comments to minTime clarifying the unit
				prommodel.TimeFromUnixNano(ds.MaxTime*1e6),
				profiles,
				tsdb,
				symbols,
			)
		}
	}

	res := make(tenantClients)
	for _, meta := range blocks {
		for _, ds := range meta.Datasets {
			tenant := meta.StringTable[ds.Tenant]
			if _, ok := res[tenant]; !ok {
				// todo consider not using BlockQuerier for tests
				blockBucket, err := filesystem.NewBucket(filepath.Join(dir, tenant))
				require.NoError(sw.t, err)

				blockQuerier := phlaredb.NewBlockQuerier(context.Background(), blockBucket)
				err = blockQuerier.Sync(context.Background())
				require.NoError(sw.t, err)

				queriers := blockQuerier.Queriers()
				err = queriers.Open(context.Background())
				require.NoError(sw.t, err)

				q, f := memdbtest.IngesterClientForTest(sw.t, queriers)

				res[tenant] = tenantClient{
					tenant: tenant,
					client: q,
					f:      f,
				}
			}
		}
	}

	return res
}

func (sw *sw) queryInputs(clients tenantClients, inputs groupedInputs) {
	sw.queryNo++
	t := sw.t
	defer func() {
		for _, tc := range clients {
			tc.f()
		}
	}()

	for tenant, tenantInputs := range inputs {
		tc, ok := clients[tenant]
		require.True(sw.t, ok)
		for svc, metricNameInputs := range tenantInputs {
			for metricName, profiles := range metricNameInputs {
				start, end := getStartEndTime(profiles)
				ps := make([]*profilev1.Profile, 0, len(profiles))
				for _, p := range profiles {
					ps = append(ps, p.Profile)
				}
				expectedMerged := mergeProfiles(sw.t, ps)

				sts := sampleTypesFromMetricName(sw.t, metricName)
				for sti, st := range sts {
					q := &ingesterv1.SelectProfilesRequest{
						LabelSelector: fmt.Sprintf("{%s=\"%s\"}", model.LabelNameServiceName, svc),
						Type:          st,
						Start:         start,
						End:           end,
					}
					actualMerged := sw.query(tc, q)

					actualCollapsed := bench.StackCollapseProto(actualMerged, 0, 1)
					expectedCollapsed := bench.StackCollapseProto(expectedMerged, sti, 1)
					require.Equal(t, expectedCollapsed, actualCollapsed)
				}

			}
		}
	}
}

func (sw *sw) query(tc tenantClient, q *ingesterv1.SelectProfilesRequest) *profilev1.Profile {
	t := sw.t
	bidi := tc.client.MergeProfilesPprof(context.Background())
	err := bidi.Send(&ingesterv1.MergeProfilesPprofRequest{
		Request: q,
	})
	require.NoError(sw.t, err)

	resp, err := bidi.Receive()
	require.NoError(t, err)
	require.Nil(t, resp.Result)
	require.NotNilf(t, resp.SelectedProfiles, "res %+v", resp)
	require.NotEmpty(t, resp.SelectedProfiles.Fingerprints)
	require.NotEmpty(t, resp.SelectedProfiles.Profiles)

	nProfiles := len(resp.SelectedProfiles.Profiles)

	bools := make([]bool, nProfiles)
	for i := 0; i < nProfiles; i++ {
		bools[i] = true
	}
	require.NoError(t, bidi.Send(&ingesterv1.MergeProfilesPprofRequest{
		Profiles: bools,
	}))

	// expect empty resp to signal it is finished
	resp, err = bidi.Receive()
	require.NoError(t, err)
	require.Nil(t, resp.Result)
	require.Nil(t, resp.SelectedProfiles)

	resp, err = bidi.Receive()
	require.NoError(t, err)
	require.NotNil(t, resp.Result)

	actualMerged := &profilev1.Profile{}
	err = actualMerged.UnmarshalVT(resp.Result)
	require.NoError(t, err)
	return actualMerged
}

// millis
func getStartEndTime(profiles []*pprofth.ProfileBuilder) (int64, int64) {
	start := profiles[0].TimeNanos
	end := profiles[0].TimeNanos
	for _, p := range profiles {
		if p.TimeNanos < start {
			start = p.TimeNanos
		}
		if p.TimeNanos > end {
			end = p.TimeNanos
		}
	}
	start = start / 1e6
	end = end / 1e6
	end += 1
	return start, end
}

func (sw *sw) getMetadataDLQ() []*metastorev1.BlockMeta {
	objects := sw.bucket.Objects()
	dlqFiles := []string{}
	for s := range objects {
		if isDLQPath(s) {
			dlqFiles = append(dlqFiles, s)
		}
	}
	slices.Sort(dlqFiles)
	var metas []*metastorev1.BlockMeta
	for _, s := range dlqFiles {
		var meta = new(metastorev1.BlockMeta)
		err := meta.UnmarshalVT(objects[s])
		require.NoError(sw.t, err)
		metas = append(metas, meta)
	}
	return metas
}

// nolint: unparam
func (sw *sw) ingestChunk(t *testing.T, chunk inputChunk, expectAwaitError bool) map[segmentWaitFlushed]struct{} {
	wg := sync.WaitGroup{}
	waiterSet := make(map[segmentWaitFlushed]struct{})
	mutex := new(sync.Mutex)
	for i := range chunk {
		it := chunk[i]
		wg.Add(1)

		go func() {
			defer wg.Done()
			awaiter := sw.ingest(shardKey(it.shard), func(head segmentIngest) {
				p := it.profile.CloneVT() // important to not rewrite original profile
				head.ingest(it.tenant, p, it.profile.UUID, it.profile.Labels, it.profile.Annotations)
			})
			err := awaiter.waitFlushed(context.Background())
			if expectAwaitError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			mutex.Lock()
			waiterSet[awaiter] = struct{}{}
			mutex.Unlock()
		}()
	}
	wg.Wait()
	return waiterSet
}

type input struct {
	shard   uint32
	tenant  string
	profile *pprofth.ProfileBuilder
}

// tenant -> service -> sample
type groupedInputs map[string]map[string]map[string][]*pprofth.ProfileBuilder

type inputChunk []input

type tenantClient struct {
	tenant string
	client ingesterv1connect.IngesterServiceClient
	f      func()
}

// tenant -> block
type tenantClients map[string]tenantClient

func groupInputs(t *testing.T, chunks ...inputChunk) groupedInputs {
	shardToTenantToServiceToSampleType := make(groupedInputs)
	for _, chunk := range chunks {

		for _, in := range chunk {
			if _, ok := shardToTenantToServiceToSampleType[in.tenant]; !ok {
				shardToTenantToServiceToSampleType[in.tenant] = make(map[string]map[string][]*pprofth.ProfileBuilder)
			}
			svc := ""
			for _, lbl := range in.profile.Labels {
				if lbl.Name == model.LabelNameServiceName {
					svc = lbl.Value
				}
			}
			require.NotEmptyf(t, svc, "service name not found in labels: %v", in.profile.Labels)
			if _, ok := shardToTenantToServiceToSampleType[in.tenant][svc]; !ok {
				shardToTenantToServiceToSampleType[in.tenant][svc] = make(map[string][]*pprofth.ProfileBuilder)
			}
			metricname := ""
			for _, lbl := range in.profile.Labels {
				if lbl.Name == prommodel.MetricNameLabel {
					metricname = lbl.Value
				}
			}
			require.NotEmptyf(t, metricname, "metric name not found in labels: %v", in.profile.Labels)
			shardToTenantToServiceToSampleType[in.tenant][svc][metricname] = append(shardToTenantToServiceToSampleType[in.tenant][svc][metricname], in.profile)
		}
	}

	return shardToTenantToServiceToSampleType

}

func cpuProfile(samples int, tsMillis int, svc string, stack ...string) *pprofth.ProfileBuilder {
	return pprofth.NewProfileBuilder(int64(tsMillis*1e6)).
		CPUProfile().
		WithLabels(model.LabelNameServiceName, svc).
		WithAnnotations("test annotation").
		ForStacktraceString(stack...).
		AddSamples([]int64{int64(samples)}...)
}

func memProfile(samples int, tsMillis int, svc string, stack ...string) *pprofth.ProfileBuilder {
	v := int64(samples)
	return pprofth.NewProfileBuilder(int64(tsMillis*1e6)).
		MemoryProfile().
		WithLabels(model.LabelNameServiceName, svc).
		ForStacktraceString(stack...).
		AddSamples([]int64{v, v * 1024, v, v * 1024}...)
}

func sampleTypesFromMetricName(t *testing.T, name string) []*typesv1.ProfileType {
	if strings.Contains(name, "process_cpu") {
		return []*typesv1.ProfileType{mustParseProfileSelector(t, "process_cpu:cpu:nanoseconds:cpu:nanoseconds")}
	}
	if strings.Contains(name, "memory") {
		return []*typesv1.ProfileType{
			mustParseProfileSelector(t, "memory:alloc_objects:count:space:bytes"),
			mustParseProfileSelector(t, "memory:alloc_space:bytes:space:bytes"),
			mustParseProfileSelector(t, "memory:inuse_objects:count:space:bytes"),
			mustParseProfileSelector(t, "memory:inuse_space:bytes:space:bytes"),
		}
	}
	require.Failf(t, "unknown metric name: %s", name)
	return nil
}

func mustParseProfileSelector(t testing.TB, selector string) *typesv1.ProfileType {
	ps, err := model.ParseProfileTypeSelector(selector)
	require.NoError(t, err)
	return ps
}

func mergeProfiles(t *testing.T, profiles []*profilev1.Profile) *profilev1.Profile {
	gps := make([]*gprofile.Profile, 0, len(profiles))
	for _, profile := range profiles {
		gp := gprofileFromProtoProfile(t, profile)
		gps = append(gps, gp)
		gp.Compact()
	}
	merge, err := gprofile.Merge(gps)
	require.NoError(t, err)

	r := bytes.NewBuffer(nil)
	err = merge.WriteUncompressed(r)
	require.NoError(t, err)

	msg := &profilev1.Profile{}
	err = msg.UnmarshalVT(r.Bytes())
	require.NoError(t, err)
	return msg
}

func gprofileFromProtoProfile(t *testing.T, profile *profilev1.Profile) *gprofile.Profile {
	data, err := profile.MarshalVT()
	require.NoError(t, err)
	p, err := gprofile.ParseData(data)
	require.NoError(t, err)
	return p
}

func staticTestData() []inputChunk {
	return []inputChunk{
		{
			//todo check why it takes 10ms for each head
			{shard: 1, tenant: "t1", profile: cpuProfile(42, 480, "svc1", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: cpuProfile(13, 233, "svc1", "qwe", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: cpuProfile(13, 472, "svc1", "qwe", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: cpuProfile(13, 961, "svc1", "qwe", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: cpuProfile(13, 56, "svc1", "qwe", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: cpuProfile(13, 549, "svc1", "qwe", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: memProfile(13, 146, "svc1", "qwe", "qwe", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: memProfile(43, 866, "svc1", "asd", "zxc")},
			{shard: 1, tenant: "t1", profile: cpuProfile(07, 213, "svc2", "s3", "s2", "s1")},
			{shard: 1, tenant: "t2", profile: cpuProfile(47, 540, "svc2", "s3", "s2", "s1")},
			{shard: 1, tenant: "t2", profile: cpuProfile(77, 499, "svc3", "s3", "s2", "s1")},
			{shard: 2, tenant: "t2", profile: cpuProfile(29, 859, "svc3", "s3", "s2", "s1")},
			{shard: 2, tenant: "t2", profile: memProfile(11, 115, "svc3", "s3", "s2", "s1")},
			{shard: 4, tenant: "t2", profile: memProfile(11, 304, "svc3", "s3", "s2", "s1")},
		},
		{
			{shard: 1, tenant: "t1", profile: cpuProfile(05, 914, "svc1", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: cpuProfile(07, 290, "svc1", "qwe", "foo", "bar")},
			{shard: 1, tenant: "t1", profile: cpuProfile(24, 748, "svc2", "s3", "s2", "s1")},
			{shard: 2, tenant: "t3", profile: memProfile(23, 639, "svc3", "s3", "s2", "s1")},
			{shard: 3, tenant: "t3", profile: memProfile(23, 912, "svc3", "s3", "s2", "s1")},
			{shard: 3, tenant: "t3", profile: memProfile(33, 799, "svc3", "s2", "s1")},
		},
	}
}

type (
	testDataGenerator struct {
		seed     int64
		chunks   int
		profiles int
		shards   int
		tenants  int
		services int
	}
)

func (g testDataGenerator) generate() []inputChunk {
	r := rand.New(rand.NewSource(g.seed))
	tg := timestampGenerator{
		m: make(map[int64]struct{}),
		r: rand.New(rand.NewSource(r.Int63())),
	}
	chunks := make([]inputChunk, g.chunks)

	services := make([]string, 0, g.services)
	for i := 0; i < g.services; i++ {
		services = append(services, fmt.Sprintf("svc%d", i))
	}
	tenatns := make([]string, 0, g.tenants)
	for i := 0; i < g.tenants; i++ {
		tenatns = append(tenatns, fmt.Sprintf("t%d", i))
	}
	const nFrames = 16384
	frames := make([]string, 0, nFrames)
	for i := 0; i < nFrames; i++ {
		frames = append(frames, fmt.Sprintf("frame%d", i))
	}

	for i := range chunks {
		chunk := make(inputChunk, 0, g.profiles)
		for j := 0; j < g.profiles; j++ {
			shard := r.Intn(g.shards)
			tenant := tenatns[r.Intn(g.tenants)]
			svc := services[r.Intn(g.services)]
			stack := make([]string, 0, 3)
			for i := 0; i < 3; i++ {
				stack = append(stack, frames[r.Intn(nFrames)])
			}
			typ := r.Intn(2)
			var p *pprofth.ProfileBuilder
			nSamples := r.Intn(100)
			ts := tg.next()
			if typ == 0 {
				p = cpuProfile(nSamples+1, ts, svc, stack...)
			} else {
				p = memProfile(nSamples+1, ts, svc, stack...)
			}
			chunk = append(chunk, input{shard: uint32(shard), tenant: tenant, profile: p})
		}
		chunks[i] = chunk
	}
	return chunks
}

type timestampGenerator struct {
	m map[int64]struct{}
	r *rand.Rand
}

func (g *timestampGenerator) next() int {
	for {
		ts := g.r.Int63n(100000000)
		if _, ok := g.m[ts]; !ok {
			g.m[ts] = struct{}{}
			return int(ts)
		}
	}
}

func isDLQPath(p string) bool {
	fs := strings.Split(p, "/")
	return len(fs) == 5 &&
		fs[0] == block.DirNameDLQ &&
		fs[2] == block.DirNameAnonTenant &&
		fs[4] == block.FileNameMetadataObject
}

func isSegmentPath(p string) bool {
	fs := strings.Split(p, "/")
	return len(fs) == 5 &&
		fs[0] == block.DirNameSegment &&
		fs[2] == block.DirNameAnonTenant &&
		fs[4] == block.FileNameDataObject
}

func hasUnsymbolizedLabel(t *testing.T, block *metastorev1.BlockMeta) bool {
	t.Helper()
	for _, ds := range block.Datasets {
		i := 0
		for i < len(ds.Labels) {
			n := int(ds.Labels[i])
			i++
			for j := 0; j < n; j++ {
				keyIdx := ds.Labels[i]
				valIdx := ds.Labels[i+1]
				i += 2
				if block.StringTable[keyIdx] == metadata.LabelNameUnsymbolized && block.StringTable[valIdx] == "true" {
					return true
				}
			}
		}
	}
	return false
}

type mockBucket struct {
	*memory.InMemBucket
	uploads atomic.Int64
}

func (m *mockBucket) Upload(ctx context.Context, _ string, _ io.Reader) error {
	m.uploads.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func TestUploadBlock_HedgedUploadLimiter(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		t.Parallel()

		bucket := &mockBucket{InMemBucket: memory.NewInMemBucket()}
		logger := test.NewTestingLogger(t)

		var config Config
		config.RegisterFlags(flag.NewFlagSet("test", flag.PanicOnError))
		config.UploadTimeout = time.Millisecond * 250
		config.UploadHedgeAfter = time.Millisecond
		config.UploadHedgeRateMax = 0
		config.UploadHedgeRateBurst = 0
		config.UploadMaxRetries = 0

		sw := &segmentsWriter{
			config:              config,
			logger:              logger,
			bucket:              bucket,
			hedgedUploadLimiter: rate.NewLimiter(rate.Limit(config.UploadHedgeRateMax), int(config.UploadHedgeRateBurst)),
			metrics:             newSegmentMetrics(nil),
		}

		err := sw.uploadBlock(context.Background(), nil, new(metastorev1.BlockMeta), new(segment))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, int64(1), bucket.uploads.Load())
	})

	t.Run("available", func(t *testing.T) {
		t.Parallel()

		bucket := &mockBucket{InMemBucket: memory.NewInMemBucket()}
		logger := test.NewTestingLogger(t)

		var config Config
		config.RegisterFlags(flag.NewFlagSet("test", flag.PanicOnError))
		config.UploadTimeout = time.Millisecond * 250
		config.UploadHedgeAfter = time.Millisecond
		config.UploadHedgeRateMax = 10
		config.UploadHedgeRateBurst = 10
		config.UploadMaxRetries = 0

		sw := &segmentsWriter{
			config:              config,
			logger:              logger,
			bucket:              bucket,
			hedgedUploadLimiter: rate.NewLimiter(rate.Limit(config.UploadHedgeRateMax), int(config.UploadHedgeRateBurst)),
			metrics:             newSegmentMetrics(nil),
		}

		// To avoid flakiness: there are no guarantees that the
		// hedged request is triggered before the upload timeout
		// expiration.
		hedgedRequestTriggered := func() bool {
			bucket.uploads.Store(0)
			err := sw.uploadBlock(context.Background(), nil, new(metastorev1.BlockMeta), new(segment))
			return errors.Is(err, context.DeadlineExceeded) && int64(2) == bucket.uploads.Load()
		}

		require.Eventually(t, hedgedRequestTriggered, time.Second*10, time.Millisecond*50)
	})

	t.Run("exhausted", func(t *testing.T) {
		t.Parallel()

		bucket := &mockBucket{InMemBucket: memory.NewInMemBucket()}
		logger := test.NewTestingLogger(t)

		var config Config
		config.RegisterFlags(flag.NewFlagSet("test", flag.PanicOnError))
		config.UploadTimeout = time.Millisecond * 250
		config.UploadHedgeAfter = time.Millisecond
		config.UploadHedgeRateMax = 0.1
		config.UploadHedgeRateBurst = 10
		config.UploadMaxRetries = 0

		sw := &segmentsWriter{
			config:              config,
			logger:              logger,
			bucket:              bucket,
			hedgedUploadLimiter: rate.NewLimiter(rate.Limit(config.UploadHedgeRateMax), int(config.UploadHedgeRateBurst)),
			metrics:             newSegmentMetrics(nil),
		}

		require.True(t, sw.hedgedUploadLimiter.ReserveN(time.Now(), int(config.UploadHedgeRateBurst)).OK())
		err := sw.uploadBlock(context.Background(), nil, new(metastorev1.BlockMeta), new(segment))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, int64(1), bucket.uploads.Load())
	})
}
