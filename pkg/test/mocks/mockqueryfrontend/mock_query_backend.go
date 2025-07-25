// Code generated by mockery. DO NOT EDIT.

package mockqueryfrontend

import (
	context "context"

	mock "github.com/stretchr/testify/mock"

	queryv1 "github.com/grafana/pyroscope/api/gen/proto/go/query/v1"
)

// MockQueryBackend is an autogenerated mock type for the QueryBackend type
type MockQueryBackend struct {
	mock.Mock
}

type MockQueryBackend_Expecter struct {
	mock *mock.Mock
}

func (_m *MockQueryBackend) EXPECT() *MockQueryBackend_Expecter {
	return &MockQueryBackend_Expecter{mock: &_m.Mock}
}

// Invoke provides a mock function with given fields: ctx, req
func (_m *MockQueryBackend) Invoke(ctx context.Context, req *queryv1.InvokeRequest) (*queryv1.InvokeResponse, error) {
	ret := _m.Called(ctx, req)

	if len(ret) == 0 {
		panic("no return value specified for Invoke")
	}

	var r0 *queryv1.InvokeResponse
	var r1 error
	if rf, ok := ret.Get(0).(func(context.Context, *queryv1.InvokeRequest) (*queryv1.InvokeResponse, error)); ok {
		return rf(ctx, req)
	}
	if rf, ok := ret.Get(0).(func(context.Context, *queryv1.InvokeRequest) *queryv1.InvokeResponse); ok {
		r0 = rf(ctx, req)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*queryv1.InvokeResponse)
		}
	}

	if rf, ok := ret.Get(1).(func(context.Context, *queryv1.InvokeRequest) error); ok {
		r1 = rf(ctx, req)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// MockQueryBackend_Invoke_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'Invoke'
type MockQueryBackend_Invoke_Call struct {
	*mock.Call
}

// Invoke is a helper method to define mock.On call
//   - ctx context.Context
//   - req *queryv1.InvokeRequest
func (_e *MockQueryBackend_Expecter) Invoke(ctx interface{}, req interface{}) *MockQueryBackend_Invoke_Call {
	return &MockQueryBackend_Invoke_Call{Call: _e.mock.On("Invoke", ctx, req)}
}

func (_c *MockQueryBackend_Invoke_Call) Run(run func(ctx context.Context, req *queryv1.InvokeRequest)) *MockQueryBackend_Invoke_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(context.Context), args[1].(*queryv1.InvokeRequest))
	})
	return _c
}

func (_c *MockQueryBackend_Invoke_Call) Return(_a0 *queryv1.InvokeResponse, _a1 error) *MockQueryBackend_Invoke_Call {
	_c.Call.Return(_a0, _a1)
	return _c
}

func (_c *MockQueryBackend_Invoke_Call) RunAndReturn(run func(context.Context, *queryv1.InvokeRequest) (*queryv1.InvokeResponse, error)) *MockQueryBackend_Invoke_Call {
	_c.Call.Return(run)
	return _c
}

// NewMockQueryBackend creates a new instance of MockQueryBackend. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewMockQueryBackend(t interface {
	mock.TestingT
	Cleanup(func())
}) *MockQueryBackend {
	mock := &MockQueryBackend{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
