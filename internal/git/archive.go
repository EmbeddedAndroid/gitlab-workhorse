/*
In this file we handle 'git archive' downloads
*/

package git

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"gitlab.com/gitlab-org/gitlab-workhorse/internal/helper"
	"gitlab.com/gitlab-org/gitlab-workhorse/internal/senddata"

	"github.com/prometheus/client_golang/prometheus"
)

type archive struct{ senddata.Prefix }
type archiveParams struct {
	RepoPath      string
	ArchivePath   string
	ArchivePrefix string
	CommitId      string
}

var (
	SendArchive     = &archive{"git-archive:"}
	gitArchiveCache = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitlab_workhorse_git_archive_cache",
			Help: "Cache hits and misses for 'git archive' streaming",
		},
		[]string{"result"},
	)
)

func init() {
	prometheus.MustRegister(gitArchiveCache)
}

func (a *archive) Inject(w http.ResponseWriter, r *http.Request, sendData string) {
	var params archiveParams
	if err := a.Unpack(&params, sendData); err != nil {
		helper.Fail500(w, r, fmt.Errorf("SendArchive: unpack sendData: %v", err))
		return
	}

	urlPath := r.URL.Path
	format, ok := parseBasename(filepath.Base(urlPath))
	if !ok {
		helper.Fail500(w, r, fmt.Errorf("SendArchive: invalid format: %s", urlPath))
		return
	}

	archiveFilename := path.Base(params.ArchivePath)

	if cachedArchive, err := os.Open(params.ArchivePath); err == nil {
		defer cachedArchive.Close()
		gitArchiveCache.WithLabelValues("hit").Inc()
		setArchiveHeaders(w, format, archiveFilename)
		// Even if somebody deleted the cachedArchive from disk since we opened
		// the file, Unix file semantics guarantee we can still read from the
		// open file in this process.
		http.ServeContent(w, r, "", time.Unix(0, 0), cachedArchive)
		return
	}

	gitArchiveCache.WithLabelValues("miss").Inc()

	// We assume the tempFile has a unique name so that concurrent requests are
	// safe. We create the tempfile in the same directory as the final cached
	// archive we want to create so that we can use an atomic link(2) operation
	// to finalize the cached archive.
	tempFile, err := prepareArchiveTempfile(path.Dir(params.ArchivePath), archiveFilename)
	if err != nil {
		helper.Fail500(w, r, fmt.Errorf("SendArchive: create tempfile: %v", err))
		return
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())

	archiveReader, err := newArchiveReader(r.Context(), params.RepoPath, format, params.ArchivePrefix, params.CommitId)
	if err != nil {
		helper.Fail500(w, r, err)
		return
	}

	reader := io.TeeReader(archiveReader, tempFile)

	// Start writing the response
	setArchiveHeaders(w, format, archiveFilename)
	w.WriteHeader(200) // Don't bother with HTTP 500 from this point on, just return
	if _, err := io.Copy(w, reader); err != nil {
		helper.LogError(r, &copyError{fmt.Errorf("SendArchive: copy 'git archive' output: %v", err)})
		return
	}

	if err := finalizeCachedArchive(tempFile, params.ArchivePath); err != nil {
		helper.LogError(r, fmt.Errorf("SendArchive: finalize cached archive: %v", err))
		return
	}
}

func setArchiveHeaders(w http.ResponseWriter, format ArchiveFormat, archiveFilename string) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveFilename))
	if format == ZipFormat {
		w.Header().Set("Content-Type", "application/zip")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Transfer-Encoding", "binary")
	w.Header().Set("Cache-Control", "private")
}

func prepareArchiveTempfile(dir string, prefix string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return ioutil.TempFile(dir, prefix)
}

func finalizeCachedArchive(tempFile *os.File, archivePath string) error {
	if err := tempFile.Close(); err != nil {
		return err
	}
	if err := os.Link(tempFile.Name(), archivePath); err != nil && !os.IsExist(err) {
		return err
	}

	return nil
}

func parseBasename(basename string) (ArchiveFormat, bool) {
	var format ArchiveFormat

	switch basename {
	case "archive.zip":
		format = ZipFormat
	case "archive.tar":
		format = TarFormat
	case "archive", "archive.tar.gz", "archive.tgz", "archive.gz":
		format = TarGzFormat
	case "archive.tar.bz2", "archive.tbz", "archive.tbz2", "archive.tb2", "archive.bz2":
		format = TarBz2Format
	default:
		return InvalidFormat, false
	}

	return format, true
}
