package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/hashicorp/nomad/api"
)

func sum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// objects serves data by path over plain http, which the dev agent's go-getter can reach without a trusted certificate;
// the plugin fetches whatever URL the core has checked, so the scheme does not matter here. /moved/ redirects to the
// object of the rest of its path. It records each request's method and path.
type objects struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
}

func newObjects(t *testing.T, data map[string][]byte) *objects {
	t.Helper()
	o := &objects{}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.requests = append(o.requests, r.Method+" "+r.URL.Path)
		o.mu.Unlock()
		if moved, ok := strings.CutPrefix(r.URL.Path, "/moved"); ok {
			http.Redirect(w, r, moved, http.StatusFound)
			return
		}
		content, ok := data[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(content)
	}))
	t.Cleanup(o.server.Close)
	return o
}

func (o *objects) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.requests...)
}

func TestBuildJobFetchesEachInputAsAnArtifact(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("fetches")
	run.Fetches = []wherry.Fetch{{Name: "data.tar.gz", URL: "https://store.example.test/b/k?X-Amz-Signature=1", Size: 3, SHA256: strings.Repeat("a", 64)}}
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID, run.Turn))
	if err != nil {
		t.Fatal(err)
	}
	want := []*api.TaskArtifact{{
		GetterSource: new(run.Fetches[0].URL), GetterOptions: map[string]string{"checksum": "sha256:" + strings.Repeat("a", 64), "archive": "false"},
		GetterMode: new("file"), RelativeDest: new("local/inputs/data.tar.gz"),
	}}
	if got := job.TaskGroups[0].Tasks[0].Artifacts; !reflect.DeepEqual(got, want) {
		t.Fatalf("artifacts %+v, want %+v", got, want)
	}
}

// Fetches arrive byte for byte under their names, an archive's name included: archive=false keeps go-getter from
// unpacking it.
func TestFetchesArriveVerifiedOnNomad(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `set -e; mkdir -p "$WHERRY_OUTPUT_DIR"; cp "$WHERRY_INPUT_DIR"/* "$WHERRY_OUTPUT_DIR"/; echo '{"type":"result","result":"done"}'`)
	dataset, archive := bytes.Repeat([]byte("dataset "), 4096), []byte("not a gzip stream")
	store := newObjects(t, map[string][]byte{"/b/dataset.bin": dataset, "/b/data.tar.gz": archive})
	run := testRun("fetches")
	run.Fetches = []wherry.Fetch{
		{Name: "dataset.bin", URL: store.server.URL + "/b/dataset.bin?X-Amz-Signature=abc", Size: int64(len(dataset)), SHA256: sum(dataset)},
		{Name: "data.tar.gz", URL: store.server.URL + "/b/data.tar.gz?X-Amz-Signature=abc", Size: int64(len(archive)), SHA256: sum(archive)},
	}
	outputs := runToOutputs(t, adapter, ctx, run)
	if !bytes.Equal(outputs["dataset.bin"].Data, dataset) || !bytes.Equal(outputs["data.tar.gz"].Data, archive) {
		t.Fatalf("fetched %d and %d bytes; want both inputs whole", len(outputs["dataset.bin"].Data), len(outputs["data.tar.gz"].Data))
	}
}

func TestAFetchThatDoesNotVerifyNeverStartsOnNomad(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `echo '{"type":"result","result":"started"}'`)
	store := newObjects(t, map[string][]byte{"/b/dataset.bin": []byte("tampered")})
	run := testRun("tampered")
	run.Fetches = []wherry.Fetch{{Name: "dataset.bin", URL: store.server.URL + "/b/dataset.bin", Size: 8, SHA256: sum([]byte("original"))}}
	handle := dispatch(t, adapter, ctx, run)
	terminal, err := adapter.Follow(ctx, handle, 0, make(chan wherry.Line, 16))
	if err != nil || terminal.Started || terminal.Class != wherry.ClassFetch {
		t.Fatalf("terminal %+v, %v; want never started with class %s", terminal, err, wherry.ClassFetch)
	}
	if job, err := adapter.Lookup(ctx, run.ID, run.Turn); err != nil || job.State != wherry.JobNeverStarted {
		t.Fatalf("lookup = %+v, %v; want never started", job, err)
	}
}

// Measured on Nomad 2.0.7: go-getter follows a redirect, and no artifact option refuses one; only the checksum holds
// the content. If this test fails, Nomad has learned to refuse redirects, and the plugin should ask it to.
func TestNomadFollowsAFetchsRedirect(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `set -e; mkdir -p "$WHERRY_OUTPUT_DIR"; cp "$WHERRY_INPUT_DIR/dataset.bin" "$WHERRY_OUTPUT_DIR/"; echo '{"type":"result","result":"done"}'`)
	dataset := []byte("dataset")
	store := newObjects(t, map[string][]byte{"/b/dataset.bin": dataset})
	run := testRun("redirect")
	run.Fetches = []wherry.Fetch{{Name: "dataset.bin", URL: store.server.URL + "/moved/b/dataset.bin", Size: int64(len(dataset)), SHA256: sum(dataset)}}
	if got := runToOutputs(t, adapter, ctx, run)["dataset.bin"].Data; !bytes.Equal(got, dataset) {
		t.Fatalf("fetched %q", got)
	}
	if seen := store.seen(); !reflect.DeepEqual(seen, []string{"GET /moved/b/dataset.bin", "GET /b/dataset.bin"}) {
		t.Fatalf("requests %q; want the redirect followed with one GET each", seen)
	}
}

// A streamed output's Body reads the file whole as the receiver of the Output reads it; the others are read as before.
func TestCollectStreamsOnNomad(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `set -e; o="$WHERRY_OUTPUT_DIR"; mkdir -p "$o"; yes b | head -c 3145728 > "$o/big.bin"; echo small > "$o/small.txt"; echo '{"type":"result","result":"done"}'`)
	run := testRun("stream")
	handle := dispatch(t, adapter, ctx, run)
	output(t, ctx, adapter, handle)
	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
	outputs := make(chan wherry.Output)
	ended := make(chan error, 1)
	go func() {
		defer close(outputs)
		ended <- adapter.Collect(ctx, handle, wherry.Limits{Files: 256, FileBytes: 16, TotalBytes: 16, Stream: []string{"big.bin"}, StreamBytes: 4 << 20}, outputs)
	}()
	got := map[string][]byte{}
	for output := range outputs {
		data := output.Data
		if output.Body != nil {
			var err error
			data, err = io.ReadAll(output.Body)
			output.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		got[output.Name] = data
	}
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got["big.bin"], bytes.Repeat([]byte("b\n"), 3145728/2)) || string(got["small.txt"]) != "small\n" {
		t.Fatalf("collected %d bytes of big.bin and %q", len(got["big.bin"]), got["small.txt"])
	}
}
