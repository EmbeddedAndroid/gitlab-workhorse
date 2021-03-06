package git

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"syscall"

	"gitlab.com/gitlab-org/gitlab-workhorse/internal/helper"
)

type ArchiveFormat int

const (
	InvalidFormat ArchiveFormat = iota
	ZipFormat
	TarFormat
	TarGzFormat
	TarBz2Format
)

func parseArchiveFormat(format ArchiveFormat) (*exec.Cmd, string) {
	switch format {
	case TarFormat:
		return nil, "tar"
	case TarGzFormat:
		return exec.Command("gzip", "-c", "-n"), "tar"
	case TarBz2Format:
		return exec.Command("bzip2", "-c"), "tar"
	case ZipFormat:
		return nil, "zip"
	default:
		return nil, "invalid format"
	}
}

type archiveReader struct {
	waitCmds []*exec.Cmd
	stdout   io.Reader
}

func (a *archiveReader) Read(p []byte) (int, error) {
	n, err := a.stdout.Read(p)

	if err != io.EOF {
		return n, err
	}

	err = a.wait()
	if err == nil {
		err = io.EOF
	}
	return n, err
}

func (a *archiveReader) wait() error {
	var waitErrors []error

	// Must call Wait() on _all_ commands
	for _, cmd := range a.waitCmds {
		waitErrors = append(waitErrors, cmd.Wait())
	}

	for _, err := range waitErrors {
		if err != nil {
			return err
		}
	}
	return nil
}

func newArchiveReader(ctx context.Context, repoPath string, format ArchiveFormat, archivePrefix string, commitId string) (a *archiveReader, err error) {
	a = &archiveReader{}

	compressCmd, formatArg := parseArchiveFormat(format)
	archiveCmd := gitCommand("", "", "git", "--git-dir="+repoPath, "archive", "--format="+formatArg, "--prefix="+archivePrefix+"/", commitId)

	var archiveStdout io.ReadCloser
	archiveStdout, err = archiveCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("SendArchive: archive stdout: %v", err)
	}
	defer func() {
		if err != nil {
			archiveStdout.Close()
		}
	}()

	a.stdout = archiveStdout

	if compressCmd != nil {
		compressCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		compressCmd.Stdin = archiveStdout

		var compressStdout io.ReadCloser
		compressStdout, err = compressCmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("SendArchive: compress stdout: %v", err)
		}
		defer func() {
			if err != nil {
				compressStdout.Close()
			}
		}()

		if err := compressCmd.Start(); err != nil {
			return nil, fmt.Errorf("SendArchive: start %v: %v", compressCmd.Args, err)
		}

		go ctxKill(ctx, compressCmd)
		a.waitCmds = append(a.waitCmds, compressCmd)

		a.stdout = compressStdout
		archiveStdout.Close()
	}

	if err := archiveCmd.Start(); err != nil {
		return nil, fmt.Errorf("SendArchive: start %v: %v", archiveCmd.Args, err)
	}

	go ctxKill(ctx, archiveCmd)
	a.waitCmds = append(a.waitCmds, archiveCmd)

	return a, nil
}

func ctxKill(ctx context.Context, cmd *exec.Cmd) {
	<-ctx.Done()
	helper.CleanUpProcessGroup(cmd)
	cmd.Wait()
}
