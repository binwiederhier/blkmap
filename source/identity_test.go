package source

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "img")
	require.NoError(t, os.WriteFile(path, pattern(1<<20), 0600))
	open := func(off, size int64) string {
		f, err := OpenFile(path, off, size)
		require.NoError(t, err)
		defer f.Close()
		return Identity(f)
	}
	a := open(0, 0)
	assert.NotEmpty(t, a)
	assert.Equal(t, a, open(0, 0), "stable across opens")
	assert.NotEqual(t, a, open(4096, 0), "the window is part of it")
	// Rewriting the file (a new image in its place) changes it
	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, later, later))
	assert.NotEqual(t, a, open(0, 0))
}

func TestIdentityHTTP(t *testing.T) {
	t.Parallel()
	data := pattern(64 << 10)
	etag := `"v1"`
	modified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		http.ServeContent(w, r, "img", modified, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	open := func() string {
		h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
		require.NoError(t, err)
		defer h.Close()
		return Identity(h)
	}
	v1 := open()
	assert.Contains(t, v1, `"v1"`)
	etag = `"v2"`
	assert.NotEqual(t, v1, open())
	// Without an ETag, Last-Modified stands in
	etag = ""
	assert.Contains(t, open(), "2026")
}

func TestIdentityComposites(t *testing.T) {
	t.Parallel()
	slow, other := NewZero(1<<20), NewZero(2<<20)
	assert.Equal(t, Identity(slow), Identity(NewCache(other, slow)), "a cache tier is not part of the content")
	c1, err := NewConcat([]*Segment{{Offset: 0, Source: NewZero(1 << 20)}, {Offset: 1 << 20, Source: NewZero(1 << 20)}}, 0)
	require.NoError(t, err)
	c2, err := NewConcat([]*Segment{{Offset: 0, Source: NewZero(2 << 20)}}, 0)
	require.NoError(t, err)
	assert.NotEqual(t, Identity(c1), Identity(c2), "the layout is part of it")
	m1, err := NewMap([]Range{{Offset: 0, Length: 4096}})
	require.NoError(t, err)
	m2, err := NewMap([]Range{{Offset: 0, Length: 8192}})
	require.NoError(t, err)
	assert.NotEqual(t, Identity(WithMap(NewZero(1<<20), m1, 0)), Identity(WithMap(NewZero(1<<20), m2, 0)))
	assert.Equal(t, Identity(NewZero(1<<20)), Identity(NewReadAhead(NewZero(1<<20))))
	assert.Equal(t, Identity(NewZero(1<<20)), Identity(NewSwappable(NewZero(1<<20))))
	r1, err := NewRAID5([]Source{NewZero(1 << 20), nil, NewZero(1 << 20)}, 65536, LeftSymmetric, 0)
	require.NoError(t, err)
	r2, err := NewRAID5([]Source{NewZero(1 << 20), NewZero(1 << 20), NewZero(1 << 20)}, 65536, LeftSymmetric, 0)
	require.NoError(t, err)
	// The members are part of the identity (reordering them changes the content), so a
	// degraded array is a different identity: an overlay made over the full array needs
	// `blkmap pin` to be used over it
	assert.NotEqual(t, Identity(r1), Identity(r2))
	r3, err := NewRAID5([]Source{NewZero(1 << 20), NewZero(1 << 20), NewZero(1 << 20)}, 65536, LeftSymmetric, 0)
	require.NoError(t, err)
	assert.Equal(t, Identity(r2), Identity(r3))
	// A source that cannot tell its version still has a stable identity
	assert.Equal(t, Identity(&mem{data: make([]byte, 10)}), Identity(&mem{data: make([]byte, 10)}))
}

// From the 2026-10-05 external review (finding 03): two different resources can answer with
// the same size and ETag (RFC 9110 scopes a validator to its resource), so the identity must
// name the resource. Credentials and the query string are not part of it: signed URLs and
// rotated tokens keep their identity.
func TestIdentityHTTPScopesResource(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"revision-1"`)
		value := byte(1)
		if r.URL.Path == "/b" {
			value = 2
		}
		http.ServeContent(w, r, "img", time.Time{}, bytes.NewReader(bytes.Repeat([]byte{value}, 4096)))
	}))
	t.Cleanup(srv.Close)
	identity := func(url string) string {
		h, err := NewHTTP(srv.Client(), url, 0, 0)
		require.NoError(t, err)
		defer h.Close()
		return Identity(h)
	}
	a, b := identity(srv.URL+"/a"), identity(srv.URL+"/b")
	assert.NotEqual(t, a, b, "different resources with equal validators")
	assert.Equal(t, a, identity(srv.URL+"/a?sig=rotated&token=x"), "the query string is not the resource")
	withUser := strings.Replace(srv.URL, "http://", "http://user:secret@", 1)
	assert.Equal(t, a, identity(withUser+"/a"), "credentials are not the resource")
	assert.NotContains(t, a, "secret")
	// The identity before resources were part of it, so existing overlays keep working
	assert.Equal(t, LegacyIdentity(b), LegacyIdentity(a))
	assert.NotContains(t, LegacyIdentity(a), "#r")
	c, err := NewConcat([]*Segment{{Offset: 0, Source: &Zero{size: 4096}}}, 0)
	require.NoError(t, err)
	assert.Equal(t, Identity(c), LegacyIdentity(Identity(c)), "sources without a resource are unchanged")
}
