package utils

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNotFoundCache(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = time.Now })

	notFound := errors.New(`request failed, status: 404 Not Found, body: {"error":{"code":"ResourceNotFound","message":"x"}}`)

	t.Run("holds a missing-resource error until the TTL ends", func(t *testing.T) {
		var c NotFoundCache
		c.Hold(nil, "a", http.StatusNotFound, notFound)

		now = now.Add(notFoundTTL - time.Second)
		require.ErrorIs(t, c.Get(nil, "a"), notFound)
		require.ErrorContains(t, c.Get(nil, "a"), "sends no request to Azure")
		require.NoError(t, c.Get(nil, "b"))

		now = now.Add(time.Second)
		require.NoError(t, c.Get(nil, "a"))
	})

	tests := map[string]struct {
		status int
		err    error
	}{
		"another status":     {http.StatusForbidden, notFound},
		"another 404 code":   {http.StatusNotFound, errors.New(`{"error":{"code":"InvalidResourceNamespace"}}`)},
		"a longer code name": {http.StatusNotFound, errors.New(`{"error":{"code":"ResourceNotFoundInCache"}}`)},
	}
	for name, tt := range tests {
		t.Run(name+" is not held", func(t *testing.T) {
			var c NotFoundCache
			c.Hold(nil, "a", tt.status, tt.err)
			require.NoError(t, c.Get(nil, "a"))
		})
	}
}
