package helper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pod lays out a pod's volumes below a temporary root, the emptyDir roots setgid and open to all as the kubelet
// leaves them.
func pod(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{materialDir, workDir, "run/wherry/credentials", "run/wherry/hold", "proc"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{workDir, "run/wherry/credentials"} {
		if err := os.Chmod(filepath.Join(root, dir), 0o777|fs.ModeSetgid); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func material(t *testing.T, root string, keys map[string]string) {
	t.Helper()
	for key, value := range keys {
		if err := os.WriteFile(filepath.Join(root, materialDir, key), []byte(value), 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

func invoke(root string, stdin io.Reader, signal func(int, syscall.Signal) error, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	code := Run(Env{Root: root, Stdin: stdin, Stdout: &stdout, Stderr: &stderr, Signal: signal}, append([]string{Protocol}, args...))
	return code, stdout.String(), stderr.String()
}

func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode() & (fs.ModePerm | fs.ModeSetgid)
}

type awaited struct {
	code   int
	stderr string
}

// await starts the init container's wait for fetches and returns once it waits.
func await(t *testing.T, root string, fetches ...string) <-chan awaited {
	t.Helper()
	ended := make(chan awaited, 1)
	go func() {
		code, _, stderr := invoke(root, nil, nil, append([]string{"inputs", "--await"}, fetches...)...)
		ended <- awaited{code, stderr}
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(root, coordination, "lock")); err == nil {
			return ended
		}
		if time.Now().After(deadline) {
			t.Fatal("the init container never waited")
		}
	}
}

func ended(t *testing.T, wait <-chan awaited) awaited {
	t.Helper()
	select {
	case end := <-wait:
		return end
	case <-time.After(5 * time.Second):
		t.Fatal("the init container did not exit")
	}
	return awaited{}
}

func TestHelperImportsOnlyTheStandardLibrary(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".", "../cmd/wherry-k8s-helper").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, output)
	}
	allowed := []string{"github.com/benemon/wherry-plugins/kubernetes/helper", "github.com/benemon/wherry-plugins/kubernetes/cmd/wherry-k8s-helper"}
	for _, path := range strings.Fields(string(output)) {
		if !slices.Contains(allowed, path) {
			t.Fatalf("the helper depends on %s", path)
		}
	}
}

func TestHelperRefusesAnotherProtocol(t *testing.T) {
	var stderr bytes.Buffer
	if code := Run(Env{Root: pod(t), Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &stderr}, []string{"v2", "inputs"}); code != ExitProtocol || stderr.String() != "helper protocol v2 not supported" {
		t.Fatalf("exit %d, %q; want %d, helper protocol v2 not supported", code, stderr.String(), ExitProtocol)
	}
}

func TestInputsWritesCredentialsOwnerOnly(t *testing.T) {
	root := pod(t)
	material(t, root, map[string]string{"prompt": "p", "credentials.id_ed25519": "key", "credentials.token": "token"})
	if code, _, stderr := invoke(root, nil, nil, "inputs"); code != 0 {
		t.Fatalf("inputs exited %d: %s", code, stderr)
	}
	dir := filepath.Join(root, credentialsDir)
	if got := mode(t, dir); got != 0o700 {
		t.Fatalf("credentials directory mode %v; want 0700", got)
	}
	for _, file := range []string{"id_ed25519", "token"} {
		if got := mode(t, filepath.Join(dir, file)); got != 0o600 {
			t.Fatalf("credential %s mode %v; want 0600", file, got)
		}
	}
	if got := mode(t, filepath.Join(root, "run/wherry/credentials")); got != 0o777|fs.ModeSetgid {
		t.Fatalf("the volume root's mode changed to %v", got)
	}
}

func TestInputsCopiesPromptAndInputs(t *testing.T) {
	root := pod(t)
	binary := string([]byte{0, 1, 2, 255})
	material(t, root, map[string]string{"prompt": "the prompt", "credential": "model-key", "inputs.data.bin": binary, "inputs.notes.md": "notes"})
	if code, _, stderr := invoke(root, nil, nil, "inputs"); code != 0 {
		t.Fatalf("inputs exited %d: %s", code, stderr)
	}
	for path, want := range map[string]string{"wherry/prompt.txt": "the prompt", "wherry/inputs/data.bin": binary, "wherry/inputs/notes.md": "notes"} {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
		if got := mode(t, filepath.Join(root, path)); got != 0o600 {
			t.Fatalf("%s mode %v; want 0600", path, got)
		}
	}
	if got := mode(t, filepath.Join(root, inputsDir)); got != 0o700 {
		t.Fatalf("inputs directory mode %v; want 0700", got)
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if data, _ := os.ReadFile(path); !strings.HasPrefix(path, filepath.Join(root, materialDir)) && bytes.Contains(data, []byte("model-key")) {
			t.Fatalf("the agent credential was copied to %s", path)
		}
		return nil
	})
}

func TestInputsAwaitCreatesTheInputsDirectory(t *testing.T) {
	root := pod(t)
	material(t, root, map[string]string{"prompt": "p"})
	wait := await(t, root, "a", "1", digest("a"))
	if got := mode(t, filepath.Join(root, inputsDir)); got != 0o700 {
		t.Fatalf("inputs directory mode %v while waiting; want 0700", got)
	}
	invoke(root, nil, nil, "inputs", "fail", "a")
	ended(t, wait)
}

func TestInputsPutVerifiesSizeAndDigest(t *testing.T) {
	root := pod(t)
	material(t, root, map[string]string{"prompt": "p"})
	wait := await(t, root, "object.bin", "5", digest("hello"))
	for _, body := range []string{"hellx", "hell", "hello!"} {
		if code, _, _ := invoke(root, strings.NewReader(body), nil, "inputs", "put", "object.bin"); code == 0 {
			t.Fatalf("put of %q accepted", body)
		}
		if _, err := os.Stat(filepath.Join(root, inputsDir, "object.bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("put of %q placed the file", body)
		}
	}
	if code, _, stderr := invoke(root, strings.NewReader("hello"), nil, "inputs", "put", "object.bin"); code != 0 {
		t.Fatalf("put exited %d: %s", code, stderr)
	}
	if got, err := os.ReadFile(filepath.Join(root, inputsDir, "object.bin")); err != nil || string(got) != "hello" || mode(t, filepath.Join(root, inputsDir, "object.bin")) != 0o600 {
		t.Fatalf("delivered %q, %v; want hello at 0600", got, err)
	}
	invoke(root, nil, nil, "inputs", "done")
	if end := ended(t, wait); end.code != 0 {
		t.Fatalf("init exited %d: %s", end.code, end.stderr)
	}
}

// unread fails the test on any read.
type unread struct{ t *testing.T }

func (u unread) Read([]byte) (int, error) {
	u.t.Error("put read a delivered file again")
	return 0, io.EOF
}

func TestInputsPutOfAVerifiedFileIsANoOp(t *testing.T) {
	root := pod(t)
	material(t, root, map[string]string{"prompt": "p"})
	wait := await(t, root, "object.bin", "5", digest("hello"))
	if code, _, stderr := invoke(root, strings.NewReader("hello"), nil, "inputs", "put", "object.bin"); code != 0 {
		t.Fatalf("put exited %d: %s", code, stderr)
	}
	if code, _, stderr := invoke(root, unread{t}, nil, "inputs", "put", "object.bin"); code != 0 {
		t.Fatalf("second put exited %d: %s", code, stderr)
	}
	invoke(root, nil, nil, "inputs", "done")
	ended(t, wait)
}

func TestInputsDoneWaitsForPuts(t *testing.T) {
	root := pod(t)
	material(t, root, map[string]string{"prompt": "p"})
	wait := await(t, root, "object.bin", "5", digest("hello"))
	body, writer := io.Pipe()
	put := make(chan int, 1)
	go func() {
		code, _, _ := invoke(root, body, nil, "inputs", "put", "object.bin")
		put <- code
	}()
	if _, err := writer.Write([]byte("he")); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _, _ := invoke(root, nil, nil, "inputs", "done")
		done <- code
	}()
	select {
	case <-done:
		t.Fatal("done returned while a put was in progress")
	case <-time.After(300 * time.Millisecond):
	}
	_, _ = writer.Write([]byte("llo"))
	writer.Close()
	if code := <-put; code != 0 {
		t.Fatalf("put exited %d", code)
	}
	if code := <-done; code != 0 {
		t.Fatalf("done exited %d", code)
	}
	if end := ended(t, wait); end.code != 0 {
		t.Fatalf("init exited %d: %s", end.code, end.stderr)
	}
}

func TestInputsAwaitTimesOut(t *testing.T) {
	defer func(idle time.Duration) { awaitIdle = idle }(awaitIdle)
	awaitIdle = 200 * time.Millisecond
	root := pod(t)
	material(t, root, map[string]string{"prompt": "p"})
	wait := await(t, root, "a", "1", digest("a"), "b", "1", digest("b"))
	if code, _, _ := invoke(root, strings.NewReader("a"), nil, "inputs", "put", "a"); code != 0 {
		t.Fatalf("put exited %d", code)
	}
	if end := ended(t, wait); end.code != ExitFetch || end.stderr != "input b: not fetched" {
		t.Fatalf("init exited %d, %q; want %d, input b: not fetched", end.code, end.stderr, ExitFetch)
	}
}

func TestInputsFailExitsWithFetchCode(t *testing.T) {
	root := pod(t)
	material(t, root, map[string]string{"prompt": "p"})
	wait := await(t, root, "a", "1", digest("a"))
	if code, _, stderr := invoke(root, nil, nil, "inputs", "fail", "a"); code != 0 {
		t.Fatalf("fail exited %d: %s", code, stderr)
	}
	if end := ended(t, wait); end.code != ExitFetch || end.stderr != "input a: not fetched" {
		t.Fatalf("init exited %d, %q; want %d, input a: not fetched", end.code, end.stderr, ExitFetch)
	}
}

func outputs(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, outputsDir)
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func entries(t *testing.T, stdout string) map[string]Entry {
	t.Helper()
	listed := map[string]Entry{}
	for line := range strings.Lines(stdout) {
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("list line %q: %v", line, err)
		}
		listed[entry.Name] = entry
	}
	return listed
}

func TestHelperListDoesNotFollowLinks(t *testing.T) {
	root := pod(t)
	dir := outputs(t, root, map[string]string{"report.md": "report", "sub/inner.txt": "inner"})
	for link, target := range map[string]string{"link.md": "report.md", "dirlink": "sub"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := invoke(root, nil, nil, "list", "100")
	if code != 0 {
		t.Fatalf("list exited %d: %s", code, stderr)
	}
	listed := entries(t, stdout)
	want := map[string]Entry{
		"report.md": {"report.md", 6, 0}, "sub": {"sub", listed["sub"].Size, fs.ModeDir}, "sub/inner.txt": {"sub/inner.txt", 5, 0},
		"link.md": {"link.md", int64(len("report.md")), fs.ModeSymlink}, "dirlink": {"dirlink", int64(len("sub")), fs.ModeSymlink},
	}
	if len(listed) != len(want) {
		t.Fatalf("listed %v; want %v", listed, want)
	}
	for name, entry := range want {
		if listed[name] != entry {
			t.Fatalf("listed %s as %+v; want %+v", name, listed[name], entry)
		}
	}
}

func TestHelperListStopsPastTheLimit(t *testing.T) {
	root := pod(t)
	files := map[string]string{}
	for i := range 10 {
		files["f"+strconv.Itoa(i)] = "x"
	}
	outputs(t, root, files)
	if code, stdout, _ := invoke(root, nil, nil, "list", "3"); code != 0 || strings.Count(stdout, "\n") != 4 {
		t.Fatalf("list 3 exited %d with %d lines; want 4", code, strings.Count(stdout, "\n"))
	}
	if code, stdout, _ := invoke(pod(t), nil, nil, "list", "3"); code != 0 || stdout != "" {
		t.Fatalf("list without an outputs directory exited %d with %q; want nothing", code, stdout)
	}
}

func readWithin(t *testing.T, root, name string, size int64) (int, string) {
	t.Helper()
	type result struct {
		code   int
		stdout string
	}
	done := make(chan result, 1)
	go func() {
		code, stdout, _ := invoke(root, nil, nil, "read", name, strconv.FormatInt(size, 10))
		done <- result{code, stdout}
	}()
	select {
	case r := <-done:
		return r.code, r.stdout
	case <-time.After(5 * time.Second):
		t.Fatalf("read %s blocked", name)
	}
	return 0, ""
}

func TestHelperReadRefusesNonRegular(t *testing.T) {
	root := pod(t)
	dir := outputs(t, root, map[string]string{"report.md": "report", "sub/inner.txt": "inner"})
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("report.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int64{"fifo": 0, "link.md": 6, "sub": 0, "report.md": 5} {
		if code, stdout := readWithin(t, root, name, size); code == 0 {
			t.Fatalf("read %s of %d bytes = %q; want refused", name, size, stdout)
		}
	}
	if code, stdout := readWithin(t, root, "report.md", 6); code != 0 || stdout != "report" {
		t.Fatalf("read report.md = %d, %q", code, stdout)
	}
}

func TestHelperReadRefusesEscapingNames(t *testing.T) {
	root := pod(t)
	dir := outputs(t, root, map[string]string{"report.md": "report"})
	if err := os.WriteFile(filepath.Join(root, workDir, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret", "up/secret", filepath.Join(root, workDir, "secret")} {
		if code, stdout := readWithin(t, root, name, 6); code == 0 {
			t.Fatalf("read %s = %q; want refused", name, stdout)
		}
	}
}

func TestHelperStopSparesTheHolder(t *testing.T) {
	root := pod(t)
	self := os.Getpid()
	for _, entry := range []string{"1", "7", "40", "41", "42", strconv.Itoa(self), "self", "net"} {
		if err := os.Mkdir(filepath.Join(root, "proc", entry), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, holdPID), []byte("7"), 0o600); err != nil {
		t.Fatal(err)
	}
	var signalled []int
	record := func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Errorf("sent %v; want SIGTERM", sig)
		}
		signalled = append(signalled, pid)
		if pid == 42 {
			return syscall.ESRCH
		}
		return nil
	}
	if code, _, stderr := invoke(root, nil, record, "stop", "TERM"); code != 0 {
		t.Fatalf("stop exited %d: %s", code, stderr)
	}
	slices.Sort(signalled)
	if !slices.Equal(signalled, []int{40, 41, 42}) {
		t.Fatalf("signalled %v; want the agent's processes 40, 41 and 42 alone", signalled)
	}
	refuse := func(int, syscall.Signal) error { return syscall.EPERM }
	if code, _, _ := invoke(root, nil, refuse, "stop", "KILL"); code == 0 {
		t.Fatal("stop succeeded with every signal refused")
	}
}

func TestHoldExitsOnRelease(t *testing.T) {
	root := pod(t)
	held := make(chan int, 1)
	go func() {
		code, _, _ := invoke(root, nil, nil, "hold")
		held <- code
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(filepath.Join(root, holdPID)); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hold wrote no pid")
		}
	}
	if code, _, stderr := invoke(root, nil, syscall.Kill, "release"); code != 0 {
		t.Fatalf("release exited %d: %s", code, stderr)
	}
	select {
	case code := <-held:
		if code != 0 {
			t.Fatalf("hold exited %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold outlived release")
	}
}
