// Code generated by MockGen. DO NOT EDIT.
// Source: ./internal/pkg/cli/deploy/lbws.go

// Package mocks is a generated GoMock package.
package mocks

import (
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
)

// MockpublicCIDRBlocksGetter is a mock of publicCIDRBlocksGetter interface.
type MockpublicCIDRBlocksGetter struct {
	ctrl     *gomock.Controller
	recorder *MockpublicCIDRBlocksGetterMockRecorder
}

// MockpublicCIDRBlocksGetterMockRecorder is the mock recorder for MockpublicCIDRBlocksGetter.
type MockpublicCIDRBlocksGetterMockRecorder struct {
	mock *MockpublicCIDRBlocksGetter
}

// NewMockpublicCIDRBlocksGetter creates a new mock instance.
func NewMockpublicCIDRBlocksGetter(ctrl *gomock.Controller) *MockpublicCIDRBlocksGetter {
	mock := &MockpublicCIDRBlocksGetter{ctrl: ctrl}
	mock.recorder = &MockpublicCIDRBlocksGetterMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockpublicCIDRBlocksGetter) EXPECT() *MockpublicCIDRBlocksGetterMockRecorder {
	return m.recorder
}

// PublicCIDRBlocks mocks base method.
func (m *MockpublicCIDRBlocksGetter) PublicCIDRBlocks() ([]string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "PublicCIDRBlocks")
	ret0, _ := ret[0].([]string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// PublicCIDRBlocks indicates an expected call of PublicCIDRBlocks.
func (mr *MockpublicCIDRBlocksGetterMockRecorder) PublicCIDRBlocks() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "PublicCIDRBlocks", reflect.TypeOf((*MockpublicCIDRBlocksGetter)(nil).PublicCIDRBlocks))
}
