package generatortools

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// BaselineRef is the commit the executor records over the imported source
// before generation (internal/executor InitializeBaseline).
const BaselineRef = "ai-sdlc-baseline"

// MaxDiffBytes bounds a candidate diff (the catalog's apply-candidate limit).
const MaxDiffBytes = 768 * 1024

// GitCandidate returns a Candidate that diffs the working tree, including new
// files, against the baseline commit. It stages into a private index file so
// the generator's own index is untouched, and refuses a diff over the bound.
func GitCandidate(gitPath, repository, scratch string) Candidate {
	return func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		index := filepath.Join(scratch, "ai-sdlc-tool-index")
		defer os.Remove(index)
		env := append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		git := func(args ...string) ([]byte, error) {
			command := exec.CommandContext(ctx, gitPath, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
				"-c", "core.quotePath=false"}, args...)...)
			command.Dir, command.Env = repository, env
			var out bytes.Buffer
			command.Stdout = &limitedBuffer{buffer: &out, limit: MaxDiffBytes + 1}
			err := command.Run()
			return out.Bytes(), err
		}
		if _, err := git("read-tree", BaselineRef); err != nil {
			return "", errors.New("baseline unavailable")
		}
		if _, err := git("add", "-A", "--", "."); err != nil {
			return "", errors.New("cannot stage the candidate")
		}
		diff, err := git("diff", "--cached", "--no-color", "--no-ext-diff", "--no-renames", BaselineRef)
		if err != nil && len(diff) <= MaxDiffBytes {
			return "", errors.New("cannot diff the candidate")
		}
		if len(diff) > MaxDiffBytes {
			return "", errors.New("candidate change exceeds 768 KiB")
		}
		return string(diff), nil
	}
}

type limitedBuffer struct {
	buffer *bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buffer.Len(); room > 0 {
		if len(p) > room {
			b.buffer.Write(p[:room])
		} else {
			b.buffer.Write(p)
		}
	}
	return len(p), nil
}

// MailboxRelay posts calls to the loopback mailbox tool route.
func MailboxRelay(wait time.Duration) Relay {
	client := &http.Client{Timeout: wait + 30*time.Second}
	return func(ctx context.Context, call []byte) (int, []byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8091/v1/ai-sdlc/tools", bytes.NewReader(call))
		if err != nil {
			return 0, nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		var body bytes.Buffer
		_, err = body.ReadFrom(&limitedReader{r: response.Body, n: 64 * 1024})
		return response.StatusCode, body.Bytes(), err
	}
}

type limitedReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("tool answer exceeds its bound")
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= n
	return n, err
}
