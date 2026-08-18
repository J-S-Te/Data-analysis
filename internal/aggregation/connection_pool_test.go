package aggregation

import (
	"errors"
	"testing"
)

type connectionPoolCloserStub struct {
	closed bool
	err    error
}

func (s *connectionPoolCloserStub) Close() error {
	s.closed = true
	return s.err
}

func TestCloseSourceConnectionPoolAlwaysCloses(t *testing.T) {
	for _, test := range []struct {
		name     string
		closeErr error
	}{
		{name: "successful close"},
		{name: "close error is logged", closeErr: errors.New("close failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool := &connectionPoolCloserStub{err: test.closeErr}
			closeSourceConnectionPool(pool, "test_source")
			if !pool.closed {
				t.Fatal("source connection pool was not closed")
			}
		})
	}
}
