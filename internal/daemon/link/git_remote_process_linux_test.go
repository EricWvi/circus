package link_test

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

const gitGroupTestMode = "CIRCUS_TEST_GIT_GROUP_MODE"

func TestGitRemoteGroupChildProcess(t *testing.T) {
	mode := os.Getenv(gitGroupTestMode)
	if mode == "" {
		t.Skip("only run as the non-subreaper subprocess")
	}
	// TestMain makes the test binary a subreaper. Clear the flag in this
	// subprocess so its test parent adopts and reaps the orphaned members.
	const prSetChildSubreaper = 36
	if _, _, err := syscall.Syscall6(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0, 0, 0, 0); err != 0 {
		t.Fatal(err)
	}
	testGitRemoteProcessGroup(t, os.Getenv("CIRCUS_TEST_GIT_GROUP_DIR"), mode)
}

func TestGitRemoteProcessGroupWithoutSubreaper(t *testing.T) {
	executable, err := os.Executable()
	requireDirectoryFixture(t, err)
	for _, mode := range gitProcessModes {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(executable, "-test.run=^TestGitRemoteGroupChildProcess$")
			cmd.Env = append(os.Environ(), gitGroupTestMode+"="+mode, "CIRCUS_TEST_GIT_GROUP_DIR="+dir, "GORACE=atexit_sleep_ms=0")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			requireDirectoryFixture(t, cmd.Start())
			done := make(chan struct{})
			var runErr error
			go func() {
				runErr = cmd.Wait()
				close(done)
			}()
			t.Cleanup(func() {
				select {
				case <-done:
				default:
					_ = cmd.Process.Kill()
					<-done
				}
				if group, _ := gitProcessIDs(dir); group > 0 && syscall.Kill(-group, 0) == nil {
					_ = syscall.Kill(-group, syscall.SIGKILL)
					reapGitTestGroup(t, group, 0)
				}
			})
			reaped := 0
			eventually(t, func() bool {
				if group, _ := gitProcessIDs(dir); group > 0 {
					reaped += reapGitTestGroup(t, group, syscall.WNOHANG)
				}
				select {
				case <-done:
					return true
				default:
					return false
				}
			})
			if runErr != nil {
				t.Fatalf("non-subreaper subprocess: %v\n%s", runErr, output.Bytes())
			}
			if mode == "pipe" && reaped != 1 {
				t.Fatalf("parent reaped %d group members, want 1", reaped)
			}
		})
	}
}

func reapGitTestGroup(t *testing.T, group, options int) int {
	t.Helper()
	count := 0
	for {
		pid, err := syscall.Wait4(-group, nil, options, nil)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.ECHILD || pid == 0 && err == nil {
			return count
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
}
