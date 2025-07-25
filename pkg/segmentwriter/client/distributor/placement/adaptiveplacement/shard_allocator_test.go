package adaptiveplacement

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_shard_allocator(t *testing.T) {
	a := &shardAllocator{
		unitSize:    10,
		min:         1,
		max:         5,
		burstWindow: 50,
		decayWindow: 50,
	}

	for i, test := range []struct {
		usage uint64
		now   int64
		want  int
	}{
		{0, 0, 1},
		{0, 1, 1},
		{5, 2, 1},
		{10, 3, 2},
		{10, 4, 2},
		{11, 5, 2},
		{20, 6, 5},
		{10, 7, 5},
		{5, 8, 5},
		{5, 9, 5},
		{5, 51, 5},
		{5, 101, 1},
		{100, 151, 5},
	} {
		require.Equal(t, test.want, a.observe(test.usage, test.now), fmt.Sprint(i))
	}
}

func Test_shard_limit(t *testing.T) {
	a := &shardAllocator{
		unitSize:    128 << 10,
		min:         1,
		max:         10,
		burstWindow: 1e9 * 10,
		decayWindow: 1e9 * 10 * 5,
	}

	var now int64
	var hi int
	for i := uint64(0); i < 100; i++ {
		old := hi
		hi = a.observe(2*a.unitSize*i, now)
		require.GreaterOrEqual(t, hi, old)
		now += 1e9 * 10
	}
}
