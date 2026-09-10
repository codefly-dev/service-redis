package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runners "github.com/codefly-dev/core/runners/base"
)

// These tests drive the native lifecycle against a real subprocess on a real
// socket. The subprocess is this test binary re-executed as a redis stand-in:
// fakeRedisModeEnv switches it into server mode before the test framework
// starts, and it is reached exactly the way production reaches redis-server —
// through a runner environment, by absolute path, with only the generated
// config file in argv.
//
// Nix itself is not exercised here: it is not installed on developer machines
// or in this repository's CI, and the ownership contract under test is about
// the process the agent spawns, not about where its binary came from.

const fakeRedisModeEnv = "CODEFLY_FAKE_REDIS"

const (
	// fakeRedisServe answers PING, like a healthy server.
	fakeRedisServe = "serve"
	// fakeRedisDeaf starts and stays up but never binds the port, so readiness
	// can only ever end in cancellation or the readiness budget.
	fakeRedisDeaf = "deaf"
	// fakeRedisExit dies immediately, like a server that cannot bind.
	fakeRedisExit = "exit"
)

func init() {
	mode := os.Getenv(fakeRedisModeEnv)
	if mode == "" {
		return
	}
	fakeRedisMain(mode)
}

// fakeRedisMain never returns: this process is a redis stand-in, not a test run.
func fakeRedisMain(mode string) {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "%s is set, so this binary runs as a redis stand-in and needs a config path; unset it to run the tests\n", fakeRedisModeEnv)
		os.Exit(2)
	}
	port, err := portFromRedisConfig(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake redis:", err)
		os.Exit(2)
	}
	// The pid line is the test's evidence of which process the agent owns. It
	// matches the redis log prefix so the production log writer parses it.
	fmt.Printf("%d:M 1 Jan 2026 00:00:00.000 * fake redis mode=%s port=%d\n", os.Getpid(), mode, port)
	os.Stdout.Sync()

	switch mode {
	case fakeRedisExit:
		os.Exit(3)
	case fakeRedisDeaf:
		select {}
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake redis: listen:", err)
		os.Exit(4)
	}
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			os.Exit(0)
		}
		go func() {
			defer conn.Close()
			buf := make([]byte, 64)
			if _, readErr := conn.Read(buf); readErr != nil {
				return
			}
			_, _ = conn.Write([]byte("+PONG\r\n"))
		}()
	}
}

func portFromRedisConfig(path string) (uint16, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		value, found := strings.CutPrefix(scanner.Text(), "port ")
		if !found {
			continue
		}
		port, convErr := strconv.ParseUint(strings.TrimSpace(value), 10, 16)
		if convErr != nil {
			return 0, convErr
		}
		return uint16(port), nil
	}
	return 0, errors.New("no port directive in config")
}

// syncBuffer collects the server's output. The runner forwards stdout and
// stderr from separate goroutines, and the test reads while they write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

var fakeRedisPidLine = regexp.MustCompile(`^(\d+):M .* fake redis mode=`)

// newFakeNativeRedis builds a nixRedis whose server is this test binary. It
// returns the handle and a free port that no one is listening on yet.
func newFakeNativeRedis(t *testing.T, mode string) (*nixRedis, uint16) {
	t.Helper()
	t.Setenv(fakeRedisModeEnv, mode)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err = os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	if err = os.Symlink(self, filepath.Join(binDir, "redis-server")); err != nil {
		t.Fatalf("link fake redis-server: %v", err)
	}
	env, err := runners.NewNativeEnvironment(context.Background(), root)
	if err != nil {
		t.Fatalf("native environment: %v", err)
	}
	if err = env.Init(context.Background()); err != nil {
		t.Fatalf("init native environment: %v", err)
	}

	port := freePort(t)
	n := &nixRedis{
		env:        env,
		serverPath: filepath.Join(binDir, "redis-server"),
		dataDir:    filepath.Join(root, "data"),
		configPath: filepath.Join(root, "redis.conf"),
		port:       port,
		password:   "hunter2",
		out:        &syncBuffer{},
	}
	if err = os.MkdirAll(n.dataDir, 0o700); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	if err = n.writeConfig(); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Cleanup(func() { reapOwnedProcessGroup(t, n) })
	return n, port
}

// lookupOwnedPID reads the pid the fake server announced, if it has announced
// one yet. The runner starts every process in its own group, so this pid is
// also the process-group id.
func lookupOwnedPID(n *nixRedis) (int, bool) {
	out, ok := n.out.(*syncBuffer)
	if !ok {
		return 0, false
	}
	for _, line := range strings.Split(out.String(), "\n") {
		match := fakeRedisPidLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		pid, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, false
		}
		return pid, true
	}
	return 0, false
}

func awaitOwnedPID(n *nixRedis) (int, bool) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := lookupOwnedPID(n); ok {
			return pid, true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, false
}

func ownedPID(t *testing.T, n *nixRedis) int {
	t.Helper()
	pid, ok := awaitOwnedPID(n)
	if !ok {
		out, _ := n.out.(*syncBuffer)
		t.Fatalf("fake redis never announced its pid; output:\n%s", out.String())
	}
	return pid
}

// reapOwnedProcessGroup is the last-resort cleanup: a test that fails partway
// must not leave the server it spawned behind.
func reapOwnedProcessGroup(t *testing.T, n *nixRedis) {
	t.Helper()
	pid, ok := lookupOwnedPID(n)
	if !ok || !processAlive(pid) {
		return
	}
	t.Errorf("test left redis pid %d running; killing its process group", pid)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func processAlive(pid int) bool {
	// Signal 0 performs the permission and existence checks without delivering
	// anything. The runner reaps its children, so a dead pid is fully gone
	// rather than a zombie this would still see.
	return syscall.Kill(pid, 0) == nil
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("redis pid %d is still running", pid)
}

// requirePortFree proves the listener was released by taking the port.
func requirePortFree(t *testing.T, port uint16) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			listener.Close()
			return
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d is still held: %v", port, lastErr)
}

func requireRedisAnswers(t *testing.T, port uint16) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial redis: %v", err)
	}
	defer conn.Close()
	if _, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		t.Fatalf("write PING: %v", err)
	}
	buf := make([]byte, 16)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	read, err := conn.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read PING reply: %v", err)
	}
	if !bytes.HasPrefix(buf[:read], []byte("+PONG")) {
		t.Fatalf("redis replied %q, want +PONG", buf[:read])
	}
}

// TestNativeLaunchRollsBackWhenReadinessIsCancelled covers the failed-Init case
// the ownership contract exists for: the server is already spawned when the
// caller gives up, so returning the error without stopping it would strand a
// process holding the assigned port with no handle left to reach it.
func TestNativeLaunchRollsBackWhenReadinessIsCancelled(t *testing.T) {
	n, port := newFakeNativeRedis(t, fakeRedisDeaf)

	// Cancel as soon as the server exists — the window the ownership contract
	// is about is exactly "spawned, not yet ready".
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spawned := make(chan struct{})
	go func() {
		if _, ok := awaitOwnedPID(n); ok {
			close(spawned)
			cancel()
		}
	}()

	start := time.Now()
	err := n.launch(ctx)
	select {
	case <-spawned:
	default:
		t.Fatal("fixture never announced a server, so nothing was cancelled; this says nothing about rollback")
	}
	if err == nil {
		t.Fatal("launch reported success against a server that never answered")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("launch error does not carry the cancellation: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= redisReadyTimeout {
		t.Fatalf("launch waited out the readiness budget (%s) instead of honouring cancellation", elapsed)
	}
	pid := ownedPID(t, n)
	t.Logf("owned pid %d on port %d, rolled back in %s", pid, port, time.Since(start))

	requireProcessGone(t, pid)
	requirePortFree(t, port)
	if n.proc != nil {
		t.Error("handle still holds a process after rollback")
	}
	if n.serverCtx.Err() == nil {
		t.Error("detached server context was not cancelled")
	}
}

// TestNativeLaunchRollsBackWhenServerExitsBeforeReady pins the other failure
// shape: the server dies on its own. Readiness must notice the exit instead of
// probing a port its corpse no longer holds until the budget runs out.
func TestNativeLaunchRollsBackWhenServerExitsBeforeReady(t *testing.T) {
	n, port := newFakeNativeRedis(t, fakeRedisExit)

	start := time.Now()
	err := n.launch(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("launch reported success against a server that exited")
	}
	if !strings.Contains(err.Error(), "exited before it became ready") {
		t.Fatalf("launch did not report the exit: %v", err)
	}
	if elapsed >= redisReadyTimeout {
		t.Fatalf("launch waited out the readiness budget (%s) after the server had already exited", elapsed)
	}
	t.Logf("owned pid %d on port %d, failed in %s: %v", ownedPID(t, n), port, elapsed, err)

	requireProcessGone(t, ownedPID(t, n))
	requirePortFree(t, port)
	if n.proc != nil {
		t.Error("handle still holds a process after rollback")
	}
}

// TestNativeStopReleasesPortAndRetainsState is the normal Stop path: execution
// ends, the port comes back, the data survives, and stopping twice is fine.
func TestNativeStopReleasesPortAndRetainsState(t *testing.T) {
	n, port := newFakeNativeRedis(t, fakeRedisServe)

	if err := n.launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	pid := ownedPID(t, n)
	t.Logf("owned pid %d on port %d", pid, port)
	requireRedisAnswers(t, port)

	if err := n.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	requireProcessGone(t, pid)
	requirePortFree(t, port)
	if n.proc != nil {
		t.Error("handle still holds a process after stop")
	}
	if err := n.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}

	for _, path := range []string{n.dataDir, n.configPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("stop did not retain %s: %v", path, err)
		}
	}
}

// TestNativeSuperviseReportsUnexpectedExit: a server killed from outside must
// be reported, not silently reported as still started.
func TestNativeSuperviseReportsUnexpectedExit(t *testing.T) {
	n, port := newFakeNativeRedis(t, fakeRedisServe)

	if err := n.launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	pid := ownedPID(t, n)
	t.Logf("owned pid %d on port %d", pid, port)

	exited := make(chan error, 1)
	n.Supervise(func(err error) { exited <- err })

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill redis: %v", err)
	}
	select {
	case err := <-exited:
		t.Logf("supervision reported the exit of pid %d: %v", pid, err)
	case <-time.After(10 * time.Second):
		t.Fatal("supervision never reported the killed server")
	}
	requirePortFree(t, port)
}

// TestNativeSuperviseIgnoresDeliberateStop: Stop is not an unexpected exit.
func TestNativeSuperviseIgnoresDeliberateStop(t *testing.T) {
	n, port := newFakeNativeRedis(t, fakeRedisServe)

	if err := n.launch(context.Background()); err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Logf("owned pid %d on port %d", ownedPID(t, n), port)

	exited := make(chan error, 1)
	n.Supervise(func(err error) { exited <- err })

	if err := n.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-exited:
		t.Fatalf("supervision reported a deliberate stop as an unexpected exit: %v", err)
	case <-time.After(time.Second):
	}
}

// A server that refuses to die cannot be built out of a real subprocess — the
// runner escalates to SIGKILL — so the failed-cleanup contract is exercised
// through stubborn, the Proc stub that refuses to confirm it stopped.
func TestNativeStopKeepsHandleWhenTerminationFails(t *testing.T) {
	n := &nixRedis{port: 6379, proc: stubborn{}}

	err := n.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), stubbornStopMessage) {
		t.Fatalf("stop error = %v, want it to report the refusal", err)
	}
	if n.proc == nil {
		t.Fatal("stop dropped the handle to a process it could not terminate")
	}
}

// stubbornStopMessage is what stubborn.Stop reports; asserting on it proves the
// cause reaches the caller rather than being swallowed.
const stubbornStopMessage = "process did not terminate"

func TestNativeInitStateIsOwnerOnly(t *testing.T) {
	// The rollback path must not widen access to the secret-bearing config.
	n, _ := newFakeNativeRedis(t, fakeRedisExit)
	if err := n.launch(context.Background()); err == nil {
		t.Fatal("launch reported success against a server that exited")
	}
	info, err := os.Stat(n.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
}

// TestNativeConfigPersistsDatasetOnShutdown pins the half of the lifecycle
// contract that the config, not the code, decides. Stop terminates the server,
// and redis only writes an RDB on SIGTERM when a save point is configured — so
// `save ""` here would turn every Stop into a silent wipe of the dataset while
// the documented contract promised retention.
func TestNativeConfigPersistsDatasetOnShutdown(t *testing.T) {
	root := t.TempDir()
	n := &nixRedis{
		dataDir:    filepath.Join(root, "data"),
		configPath: filepath.Join(root, "redis.conf"),
		port:       16379,
	}
	if err := n.writeConfig(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(n.configPath)
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	if strings.Contains(config, `save ""`) {
		t.Fatalf("config disables snapshots, so terminating the server discards the dataset:\n%s", config)
	}
	if !regexp.MustCompile(`(?m)^save \d+ \d+$`).MatchString(config) {
		t.Fatalf("config has no save point, so SIGTERM writes no RDB:\n%s", config)
	}
	// The RDB has to land in the directory the lifecycle promises to retain.
	if !strings.Contains(config, "dir "+strconv.Quote(n.dataDir)) {
		t.Fatalf("config does not point the dataset at the retained data dir:\n%s", config)
	}
}
