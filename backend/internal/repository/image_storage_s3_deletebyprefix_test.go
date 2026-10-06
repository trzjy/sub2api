package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ─── S3 mock ───

type s3DeleteCall struct {
	keys []string
}

type s3ListPage struct {
	keys      []string
	nextToken string // empty means last page (IsTruncated=false)
	serverErr bool
}

// s3ImageStorageMock emulates ListObjectsV2 pagination and DeleteObjects for
// S3ImageStorage tests, in the same style as backup_s3_store_test.go.
type s3ImageStorageMock struct {
	mu sync.Mutex

	pages []s3ListPage

	listRequests   int
	deleteRequests []s3DeleteCall
	listPrefixes   []string
	listMaxKeys    []string
	// deleteResults is returned per DeleteObjects request: confirmed keys and
	// per-object errors. When nil, every submitted key is returned as deleted.
	deleteResults *s3DeleteResult
}

type s3DeleteResult struct {
	deleted []string
	errors  []s3DeleteError
}

type s3DeleteError struct {
	key     string
	code    string
	message string
}

var s3KeyRe = regexp.MustCompile(`<Key>([^<]+)</Key>`)

func (m *s3ImageStorageMock) handler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	query := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && query.Get("list-type") == "2":
		m.listRequests++
		m.listPrefixes = append(m.listPrefixes, query.Get("prefix"))
		m.listMaxKeys = append(m.listMaxKeys, query.Get("max-keys"))
		page := m.pageForToken(query.Get("continuation-token"))
		if page.serverErr {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeS3ListResponse(w, page)
	case r.Method == http.MethodPost && query.Has("delete"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var keys []string
		for _, match := range s3KeyRe.FindAllStringSubmatch(string(body), -1) {
			keys = append(keys, match[1])
		}
		m.deleteRequests = append(m.deleteRequests, s3DeleteCall{keys: keys})
		m.writeS3DeleteResponse(w, keys)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (m *s3ImageStorageMock) pageForToken(token string) s3ListPage {
	// Page 0 serves the initial request; each continuation token maps to the
	// page whose index equals the token number.
	index := 0
	if token != "" {
		_, _ = fmt.Sscanf(token, "tok-%d", &index)
	}
	if index < 0 || index >= len(m.pages) {
		return s3ListPage{}
	}
	return m.pages[index]
}

func writeS3ListResponse(w http.ResponseWriter, page s3ListPage) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	b.WriteString(`<Name>image-bucket</Name><Prefix>announcements/</Prefix>`)
	fmt.Fprintf(&b, `<KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys>`, len(page.keys))
	if page.nextToken == "" {
		b.WriteString(`<IsTruncated>false</IsTruncated>`)
	} else {
		b.WriteString(`<IsTruncated>true</IsTruncated>`)
		fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, page.nextToken)
	}
	for _, key := range page.keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>10</Size><StorageClass>STANDARD</StorageClass></Contents>`, key)
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func (m *s3ImageStorageMock) writeS3DeleteResponse(w http.ResponseWriter, submitted []string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	if m.deleteResults == nil {
		for _, key := range submitted {
			fmt.Fprintf(&b, `<Deleted><Key>%s</Key></Deleted>`, key)
		}
	} else {
		for _, key := range m.deleteResults.deleted {
			fmt.Fprintf(&b, `<Deleted><Key>%s</Key></Deleted>`, key)
		}
		for _, e := range m.deleteResults.errors {
			fmt.Fprintf(&b, `<Error><Key>%s</Key><Code>%s</Code><Message>%s</Message></Error>`, e.key, e.code, e.message)
		}
	}
	b.WriteString(`</DeleteResult>`)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

func newMockedImageStorage(t *testing.T, mock *s3ImageStorageMock) *S3ImageStorage {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(server.Close)

	client, err := newS3Client(context.Background(), s3ClientParams{
		Endpoint:        server.URL,
		Region:          "auto",
		AccessKeyID:     "test-ak",
		SecretAccessKey: "test-sk",
		ForcePathStyle:  true,
	})
	require.NoError(t, err)
	return &S3ImageStorage{client: client, bucket: "image-bucket"}
}

// ─── DeleteByPrefix ───

// A prefix with no objects deletes nothing and stays idempotent.
func TestS3ImageStorage_DeleteByPrefix_EmptyPrefixIdempotent(t *testing.T) {
	mock := &s3ImageStorageMock{pages: []s3ListPage{{keys: nil}}}
	storage := newMockedImageStorage(t, mock)

	deleted, err := storage.DeleteByPrefix(context.Background(), "announcements/")
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.Zero(t, mock.deleteRequests, "no DeleteObjects call for an empty prefix")
}

// A single list page is deleted with one DeleteObjects batch carrying exactly
// the enumerated keys.
func TestS3ImageStorage_DeleteByPrefix_SinglePage(t *testing.T) {
	keys := []string{
		"announcements/11/a.png",
		"announcements/11/b.jpg",
		"announcements/12/c.webp",
	}
	mock := &s3ImageStorageMock{pages: []s3ListPage{{keys: keys}}}
	storage := newMockedImageStorage(t, mock)

	deleted, err := storage.DeleteByPrefix(context.Background(), "announcements/")
	require.NoError(t, err)
	require.Equal(t, 3, deleted)
	require.Len(t, mock.deleteRequests, 1)
	require.Equal(t, keys, mock.deleteRequests[0].keys)
	require.Equal(t, []string{"announcements/"}, mock.listPrefixes)
}

// More than one list page is followed via the continuation token and each page
// is deleted as its own batch.
func TestS3ImageStorage_DeleteByPrefix_PaginatesAcrossBatches(t *testing.T) {
	page1 := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		page1 = append(page1, fmt.Sprintf("announcements/7/obj-%04d.png", i))
	}
	page2 := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		page2 = append(page2, fmt.Sprintf("announcements/7/obj-%04d.png", 1000+i))
	}
	mock := &s3ImageStorageMock{pages: []s3ListPage{
		{keys: page1, nextToken: "tok-1"},
		{keys: page2},
	}}
	storage := newMockedImageStorage(t, mock)

	deleted, err := storage.DeleteByPrefix(context.Background(), "announcements/")
	require.NoError(t, err)
	require.Equal(t, 1500, deleted)
	require.Equal(t, 2, mock.listRequests)
	require.Len(t, mock.deleteRequests, 2)
	require.Equal(t, page1, mock.deleteRequests[0].keys)
	require.Equal(t, page2, mock.deleteRequests[1].keys)
}

// Per-object Errors in the DeleteObjects response must be surfaced: only
// confirmed keys count as deleted and the error names the failed key.
func TestS3ImageStorage_DeleteByPrefix_PartialFailure(t *testing.T) {
	keys := []string{"announcements/11/ok-1.png", "announcements/11/bad.png", "announcements/11/ok-2.png"}
	mock := &s3ImageStorageMock{pages: []s3ListPage{{keys: keys}}}
	mock.deleteResults = &s3DeleteResult{
		deleted: []string{"announcements/11/ok-1.png", "announcements/11/ok-2.png"},
		errors: []s3DeleteError{{
			key:     "announcements/11/bad.png",
			code:    "AccessDenied",
			message: "denied by policy",
		}},
	}
	storage := newMockedImageStorage(t, mock)

	deleted, err := storage.DeleteByPrefix(context.Background(), "announcements/")
	require.Error(t, err)
	require.Equal(t, 2, deleted, "only confirmed keys count")
	require.Contains(t, err.Error(), "announcements/11/bad.png")
}

// A list failure fails the whole call after reporting what was already deleted.
func TestS3ImageStorage_DeleteByPrefix_ListFailure(t *testing.T) {
	mock := &s3ImageStorageMock{pages: []s3ListPage{{serverErr: true}}}
	storage := newMockedImageStorage(t, mock)

	deleted, err := storage.DeleteByPrefix(context.Background(), "announcements/")
	require.Error(t, err)
	require.Zero(t, deleted)
	require.Zero(t, mock.deleteRequests)
}

// ─── HasObjectsByPrefix ───

func TestS3ImageStorage_HasObjectsByPrefix(t *testing.T) {
	t.Run("objects exist", func(t *testing.T) {
		mock := &s3ImageStorageMock{pages: []s3ListPage{{keys: []string{"announcements/1/a.png"}}}}
		storage := newMockedImageStorage(t, mock)

		has, err := storage.HasObjectsByPrefix(context.Background(), "announcements/")
		require.NoError(t, err)
		require.True(t, has)
		require.Equal(t, []string{"1"}, mock.listMaxKeys, "probe reads at most one key")
	})

	t.Run("empty prefix", func(t *testing.T) {
		mock := &s3ImageStorageMock{pages: []s3ListPage{{keys: nil}}}
		storage := newMockedImageStorage(t, mock)

		has, err := storage.HasObjectsByPrefix(context.Background(), "announcements/")
		require.NoError(t, err)
		require.False(t, has)
	})

	t.Run("probe failure is an error, not false", func(t *testing.T) {
		mock := &s3ImageStorageMock{pages: []s3ListPage{{serverErr: true}}}
		storage := newMockedImageStorage(t, mock)

		has, err := storage.HasObjectsByPrefix(context.Background(), "announcements/")
		require.Error(t, err, "fail-closed: callers must not read an error as no objects")
		require.False(t, has)
	})
}
