// Package helper is the in-pod side of the plugin: the agent pod's init container, its outputs holder, and the verbs
// the plugin execs in them.
package helper

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Protocol is the first argument of every invocation; the plugin and the helper in the pod must agree on it.
const Protocol = "v1"

// Exit codes the plugin reads.
const (
	ExitFailed   = 1
	ExitFetch    = 2
	ExitProtocol = 3
)

// The pod's paths, below Env.Root.
const (
	materialDir    = "run/wherry/material"
	workDir        = "wherry"
	inputsDir      = workDir + "/inputs"
	outputsDir     = workDir + "/outputs"
	coordination   = workDir + "/.inputs"
	credentialsDir = "run/wherry/credentials/files"
	holdPID        = "run/wherry/hold/pid"
)

const poll = 100 * time.Millisecond

// awaitIdle is how long the init container waits with no put in progress before it gives up on the fetches.
var awaitIdle = 10 * time.Minute

// Env is what an invocation runs against: the pod's filesystem below Root, its standard streams, and the signal call.
type Env struct {
	Root           string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Signal         func(pid int, sig syscall.Signal) error
}

// Entry is one line of list's output.
type Entry struct {
	Name string      `json:"name"`
	Size int64       `json:"size"`
	Mode fs.FileMode `json:"mode"`
}

// expected is one fetch the init container waits for.
type expected struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Run runs one invocation and returns its exit code; a failure's message goes to Stderr.
func Run(env Env, args []string) int {
	if len(args) == 0 || args[0] != Protocol {
		version := ""
		if len(args) > 0 {
			version = args[0]
		}
		fmt.Fprintf(env.Stderr, "helper protocol %s not supported", version)
		return ExitProtocol
	}
	code, err := run(env, args[1:])
	if err != nil {
		fmt.Fprint(env.Stderr, err.Error())
		return max(code, ExitFailed)
	}
	return code
}

func run(env Env, args []string) (int, error) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	switch {
	case verb == "inputs" && (len(args) == 1 || args[1] == "--await"):
		return env.inputs(args[1:])
	case verb == "inputs" && len(args) == 3 && args[1] == "put":
		return 0, env.put(args[2])
	case verb == "inputs" && len(args) == 2 && args[1] == "done":
		return 0, env.done()
	case verb == "inputs" && len(args) == 3 && args[1] == "fail":
		return 0, os.WriteFile(env.path(coordination, "fail"), []byte(args[2]), 0o600)
	case verb == "hold" && len(args) == 1:
		return 0, env.hold()
	case verb == "release" && len(args) == 1:
		pid, err := env.holder()
		if err != nil {
			return 0, err
		}
		return 0, env.Signal(pid, syscall.SIGTERM)
	case verb == "stop" && len(args) == 2 && (args[1] == "TERM" || args[1] == "KILL"):
		return 0, env.stop(map[string]syscall.Signal{"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL}[args[1]])
	case verb == "list" && len(args) == 2:
		limit, err := strconv.Atoi(args[1])
		if err != nil {
			return 0, errors.New("list takes a count")
		}
		return 0, env.list(limit)
	case verb == "read" && len(args) == 3:
		size, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return 0, errors.New("read takes a size")
		}
		return 0, env.read(args[1], size)
	}
	return 0, errors.New("unknown helper invocation")
}

func (env Env) path(parts ...string) string {
	return filepath.Join(append([]string{env.Root}, parts...)...)
}

// inputs copies the Secret material into the pod's volumes and, with --await, waits for the fetches to be put.
func (env Env) inputs(args []string) (int, error) {
	var fetches []expected
	if len(args) > 0 {
		if (len(args)-1)%3 != 0 {
			return 0, errors.New("--await takes a name, size and digest for each fetch")
		}
		for i := 1; i < len(args); i += 3 {
			size, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return 0, errors.New("--await takes numeric sizes")
			}
			fetches = append(fetches, expected{Name: args[i], Size: size, SHA256: args[i+2]})
		}
	}
	// The emptyDir roots belong to the kubelet; the pod's user owns only what it creates.
	for _, dir := range []string{inputsDir, credentialsDir} {
		if err := mkdir(env.path(dir)); err != nil {
			return 0, err
		}
	}
	entries, err := os.ReadDir(env.path(materialDir))
	if err != nil {
		return 0, errors.New("the material volume is not readable")
	}
	for _, entry := range entries {
		key := entry.Name()
		var target string
		switch {
		case key == "prompt":
			target = env.path(workDir, "prompt.txt")
		case strings.HasPrefix(key, "inputs."):
			target = env.path(inputsDir, strings.TrimPrefix(key, "inputs."))
		case strings.HasPrefix(key, "credentials."):
			target = env.path(credentialsDir, strings.TrimPrefix(key, "credentials."))
		default:
			continue
		}
		data, err := os.ReadFile(env.path(materialDir, key))
		if err != nil {
			return 0, fmt.Errorf("material %s is not readable", key)
		}
		if err := writeFile(target, data); err != nil {
			return 0, err
		}
	}
	if fetches == nil {
		return 0, nil
	}
	return env.await(fetches)
}

// await waits for done or fail from the plugin, or gives up after awaitIdle with no put in progress.
func (env Env) await(fetches []expected) (int, error) {
	if err := mkdir(env.path(coordination)); err != nil {
		return 0, err
	}
	manifest, _ := json.Marshal(fetches) // strings and integers always encode
	if err := os.WriteFile(env.path(coordination, "manifest"), manifest, 0o600); err != nil {
		return 0, errors.New("cannot write the fetch manifest")
	}
	lock, err := os.OpenFile(env.path(coordination, "lock"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return 0, errors.New("cannot create the put lock")
	}
	defer lock.Close()
	for busy := time.Now(); ; time.Sleep(poll) {
		if name, err := os.ReadFile(env.path(coordination, "fail")); err == nil {
			return ExitFetch, fmt.Errorf("input %s: not fetched", name)
		}
		if _, err := os.Stat(env.path(coordination, "done")); err == nil {
			return 0, os.RemoveAll(env.path(coordination))
		}
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			busy = time.Now()
		} else {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
		if time.Since(busy) > awaitIdle {
			missing := fetches[0].Name
			for _, fetch := range slices.Backward(fetches) {
				if !verified(env.path(inputsDir, fetch.Name), fetch) {
					missing = fetch.Name
				}
			}
			return ExitFetch, fmt.Errorf("input %s: not fetched", missing)
		}
	}
}

// put writes one fetch from stdin, verified, into the inputs directory; a verified file already there is kept.
func (env Env) put(name string) error {
	lock, err := os.Open(env.path(coordination, "lock"))
	if err != nil {
		return errors.New("the init container is not waiting for inputs")
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return errors.New("cannot take the put lock")
	}
	manifest, err := os.ReadFile(env.path(coordination, "manifest"))
	var fetches []expected
	if err != nil || json.Unmarshal(manifest, &fetches) != nil {
		return errors.New("the fetch manifest is not readable")
	}
	var fetch *expected
	for i := range fetches {
		if fetches[i].Name == name {
			fetch = &fetches[i]
		}
	}
	if fetch == nil {
		return fmt.Errorf("input %s is not awaited", name)
	}
	target := env.path(inputsDir, name)
	if verified(target, *fetch) {
		return nil
	}
	temp, err := os.CreateTemp(env.path(coordination), "put-")
	if err != nil {
		return errors.New("cannot create a temporary file")
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	digest := sha256.New()
	if n, err := io.CopyN(io.MultiWriter(temp, digest), env.Stdin, fetch.Size); err != nil || n != fetch.Size {
		return fmt.Errorf("input %s is shorter than its size", name)
	}
	if n, _ := env.Stdin.Read(make([]byte, 1)); n > 0 {
		return fmt.Errorf("input %s is longer than its size", name)
	}
	if hex.EncodeToString(digest.Sum(nil)) != fetch.SHA256 {
		return fmt.Errorf("input %s does not match its digest", name)
	}
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), target); err != nil {
		return fmt.Errorf("cannot place input %s", name)
	}
	return nil
}

// done returns once no put is in progress, and lets the init container exit.
func (env Env) done() error {
	lock, err := os.Open(env.path(coordination, "lock"))
	if err != nil {
		return errors.New("the init container is not waiting for inputs")
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return errors.New("cannot take the put lock")
	}
	return os.WriteFile(env.path(coordination, "done"), nil, 0o600)
}

// verified reports whether path is a regular file of the fetch's size and digest.
func verified(path string, fetch expected) bool {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != fetch.Size {
		return false
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return false
	}
	return hex.EncodeToString(digest.Sum(nil)) == fetch.SHA256
}

// mkdir creates dir at 0700; chmod clears the setgid bit a parent with an fsGroup passes on.
func mkdir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("cannot create %s", filepath.Base(dir))
	}
	return os.Chmod(dir, 0o700)
}

func writeFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write %s", filepath.Base(path))
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("cannot write %s", filepath.Base(path))
	}
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return file.Close()
}

// hold records its PID and waits for SIGTERM.
func (env Env) hold() error {
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	defer signal.Stop(terminated)
	if err := os.WriteFile(env.path(holdPID), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return errors.New("cannot write the holder's pid")
	}
	<-terminated
	return nil
}

func (env Env) holder() (int, error) {
	data, err := os.ReadFile(env.path(holdPID))
	if err != nil {
		return 0, errors.New("the holder's pid is not readable")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, errors.New("the holder's pid is not a number")
	}
	return pid, nil
}

// stop signals every process in the pod's shared namespace but the infra container's, its own and the holder's.
func (env Env) stop(sig syscall.Signal) error {
	holder, err := env.holder()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(env.path("proc"))
	if err != nil {
		return errors.New("the process list is not readable")
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == 1 || pid == os.Getpid() || pid == holder {
			continue
		}
		switch err := env.Signal(pid, sig); {
		case errors.Is(err, syscall.ESRCH):
		case err != nil:
			return fmt.Errorf("signal refused: %v", err)
		}
	}
	return nil
}

// list prints the outputs tree, lstat'd and never through a link, stopping after limit+1 entries.
func (env Env) list(limit int) error {
	root, err := os.OpenRoot(env.path(outputsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("the outputs directory is not readable")
	}
	defer root.Close()
	out := bufio.NewWriter(env.Stdout)
	encoder := json.NewEncoder(out)
	pending, listed := []string{"."}, 0
	for len(pending) > 0 {
		dir := pending[0]
		pending = pending[1:]
		file, err := root.Open(dir)
		if err != nil {
			return errors.New("an outputs directory is not readable")
		}
		names, err := file.Readdirnames(-1)
		file.Close()
		if err != nil {
			return errors.New("an outputs directory is not readable")
		}
		for _, base := range names {
			name := filepath.Join(dir, base)
			info, err := root.Lstat(name)
			if err != nil {
				return errors.New("an output is not readable")
			}
			if err := encoder.Encode(Entry{Name: filepath.ToSlash(name), Size: info.Size(), Mode: info.Mode().Type()}); err != nil {
				return err
			}
			if listed++; listed > limit {
				return out.Flush()
			}
			if info.IsDir() {
				pending = append(pending, name)
			}
		}
	}
	return out.Flush()
}

// read copies a regular file of exactly size bytes to stdout.
func (env Env) read(name string, size int64) error {
	root, err := os.OpenRoot(env.path(outputsDir))
	if err != nil {
		return errors.New("the outputs directory is not readable")
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("output %s is not readable", name)
	}
	defer file.Close()
	info, err := file.Stat()
	// The root follows a link in the name's last element whatever the flags, so the name must lstat as the file opened.
	named, namedErr := root.Lstat(name)
	if err != nil || namedErr != nil || !info.Mode().IsRegular() || !os.SameFile(info, named) || info.Size() != size {
		return fmt.Errorf("output %s is not a regular file of its listed size", name)
	}
	if _, err := io.CopyN(env.Stdout, file, size); err != nil {
		return fmt.Errorf("output %s read short", name)
	}
	return nil
}
