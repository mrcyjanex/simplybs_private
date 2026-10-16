package pack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sbsconfig "github.com/mrcyjanek/simplybs/config"
)

const (
	cachePartSizeDefault = 64 << 20
	cacheMaxParts        = 10000
	cacheKeepBatch       = 20000
)

// cachePartSize is the multipart part size. Tests lower it. Production stays
// at 64 MiB so every part except the last is above the 5 MiB minimum.
var cachePartSize int64 = cachePartSizeDefault

var (
	cacheDownloadRetries = 5
	cacheDownloadBackoff = time.Second
)

// cacheBaseURL overrides the API host in tests. Empty uses config.CacheURL.
var cacheBaseURL string

var errNotFound = errors.New("cache: object not found")

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("cache: http %d", e.code)
	}
	return fmt.Sprintf("cache: http %d: %s", e.code, e.msg)
}

type remoteObject struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	LastPingedAt int64  `json:"lastPingedAt"`
}

type sbsClient struct {
	base  string
	token string
	http  *http.Client
}

func cacheClient() (*sbsClient, error) {
	tok, err := sbsconfig.Token()
	if err != nil {
		return nil, err
	}
	base := cacheBaseURL
	if base == "" {
		base = sbsconfig.CacheURL
	}
	return &sbsClient{
		base:  strings.TrimRight(base, "/"),
		token: tok,
		http:  &http.Client{},
	}, nil
}

func pathEscape(s string) string {
	// url.PathEscape leaves '+' alone. Object names use '+' for '/', and the
	// API requires that character to be sent as %2B.
	return strings.ReplaceAll(url.PathEscape(s), "+", "%2B")
}

// cacheObjectName maps an asset name onto the sbscache alphabet
// [A-Za-z0-9._+-]. Package names use '@' (monero@fcmp-stage), which the
// server rejects. '.at.' stands for '@'. An existing '.at.' is written
// '.at.at.' first, so the mapping round-trips. Names with neither stay
// unchanged, and signed manifests keep the original '@'.
func cacheObjectName(name string) string {
	name = strings.ReplaceAll(name, ".at.", ".at.at.")
	return strings.ReplaceAll(name, "@", ".at.")
}

func cacheNameFromObject(name string) string {
	const escaped = ".at.at."
	const at = ".at."
	var b strings.Builder
	b.Grow(len(name))
	for len(name) > 0 {
		switch {
		case strings.HasPrefix(name, escaped):
			b.WriteString(at)
			name = name[len(escaped):]
		case strings.HasPrefix(name, at):
			b.WriteByte('@')
			name = name[len(at):]
		default:
			b.WriteByte(name[0])
			name = name[1:]
		}
	}
	return b.String()
}

func validCacheObjectName(name string) bool {
	if name == "" || len(name) > 512 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '+' || c == '-':
		default:
			return false
		}
	}
	return true
}

func wireObjectName(name string) (string, error) {
	wire := cacheObjectName(name)
	if !validCacheObjectName(wire) {
		return "", fmt.Errorf("cache: invalid object name %q", name)
	}
	return wire, nil
}

func (c *sbsClient) tagURL(tag string, rest string) string {
	u := c.base + "/v1/tags/" + pathEscape(tag)
	if rest != "" {
		u += rest
	}
	return u
}

func (c *sbsClient) objectURL(tag, name string) string {
	return c.tagURL(tag, "/objects/"+pathEscape(name))
}

func (c *sbsClient) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.http.Do(req)
}

func readAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	return &statusError{code: resp.StatusCode, msg: body.Error}
}

func (c *sbsClient) List(tag string) ([]remoteObject, error) {
	req, err := http.NewRequest(http.MethodGet, c.tagURL(tag, "/objects"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	var parsed struct {
		Objects []remoteObject `json:"objects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.Objects == nil {
		parsed.Objects = []remoteObject{}
	}
	for i := range parsed.Objects {
		parsed.Objects[i].Name = cacheNameFromObject(parsed.Objects[i].Name)
	}
	return parsed.Objects, nil
}

func (c *sbsClient) Upload(tag, name, path string) error {
	name, err := wireObjectName(name)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	partSize := cachePartSize
	if partSize < 1 {
		partSize = cachePartSizeDefault
	}
	nparts := int64(1)
	if size > 0 {
		nparts = (size + partSize - 1) / partSize
	}
	if nparts > cacheMaxParts {
		return fmt.Errorf("cache: %s needs %d parts (limit %d)", name, nparts, cacheMaxParts)
	}

	uploadID, err := c.createUpload(tag, name)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			_ = c.abortUpload(tag, name, uploadID)
		}
	}()

	var sent int64
	for part := int64(1); part <= nparts; part++ {
		n := partSize
		if size-sent < n {
			n = size - sent
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(f, buf); err != nil {
			return err
		}
		if err := c.putPart(tag, name, uploadID, int(part), buf); err != nil {
			return err
		}
		sent += n
	}
	if err := c.completeUpload(tag, name, uploadID); err != nil {
		return err
	}
	finished = true
	return nil
}

func (c *sbsClient) createUpload(tag, name string) (string, error) {
	var last error
	for attempt := 1; attempt <= cacheDownloadRetries; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.objectURL(tag, name)+"/uploads", nil)
		if err != nil {
			return "", err
		}
		resp, err := c.do(req)
		if err != nil {
			last = err
			if !retryable(err) || attempt == cacheDownloadRetries {
				return "", err
			}
			time.Sleep(cacheDownloadBackoff * time.Duration(1<<(attempt-1)))
			continue
		}
		if resp.StatusCode != http.StatusCreated {
			err = readAPIError(resp)
			if !retryable(err) || attempt == cacheDownloadRetries {
				return "", err
			}
			last = err
			time.Sleep(cacheDownloadBackoff * time.Duration(1<<(attempt-1)))
			continue
		}
		var parsed struct {
			UploadID string `json:"uploadId"`
		}
		err = json.NewDecoder(resp.Body).Decode(&parsed)
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		if parsed.UploadID == "" {
			return "", fmt.Errorf("cache: empty upload id for %s", name)
		}
		return parsed.UploadID, nil
	}
	return "", last
}

func (c *sbsClient) putPart(tag, name, uploadID string, part int, body []byte) error {
	u := c.objectURL(tag, name) + "/uploads/" + pathEscape(uploadID) + "/parts/" + strconv.Itoa(part)
	var last error
	for attempt := 1; attempt <= cacheDownloadRetries; attempt++ {
		req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(body))
		resp, err := c.do(req)
		if err != nil {
			last = err
			if !retryable(err) || attempt == cacheDownloadRetries {
				return err
			}
			time.Sleep(cacheDownloadBackoff * time.Duration(1<<(attempt-1)))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			err = readAPIError(resp)
			if !retryable(err) || attempt == cacheDownloadRetries {
				return err
			}
			last = err
			time.Sleep(cacheDownloadBackoff * time.Duration(1<<(attempt-1)))
			continue
		}
		resp.Body.Close()
		return nil
	}
	return last
}

func (c *sbsClient) completeUpload(tag, name, uploadID string) error {
	u := c.objectURL(tag, name) + "/uploads/" + pathEscape(uploadID) + "/complete"
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return readAPIError(resp)
	}
	resp.Body.Close()
	return nil
}

func (c *sbsClient) abortUpload(tag, name, uploadID string) error {
	u := c.objectURL(tag, name) + "/uploads/" + pathEscape(uploadID)
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return readAPIError(resp)
	}
	return nil
}

func (c *sbsClient) Download(tag, name, dest string) error {
	name, err := wireObjectName(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	tmp := dest + ".partial"
	var last error
	for attempt := 1; attempt <= cacheDownloadRetries; attempt++ {
		err := c.downloadOnce(tag, name, tmp)
		if err == nil {
			return os.Rename(tmp, dest)
		}
		last = err
		if !retryable(err) {
			os.Remove(tmp)
			return err
		}
		if attempt < cacheDownloadRetries {
			time.Sleep(cacheDownloadBackoff * time.Duration(1<<(attempt-1)))
		}
	}
	return last
}

func (c *sbsClient) downloadOnce(tag, name, tmp string) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	off := st.Size()
	req, err := http.NewRequest(http.MethodGet, c.objectURL(tag, name), nil)
	if err != nil {
		return err
	}
	if off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case http.StatusOK:
		if off > 0 {
			if err := f.Truncate(0); err != nil {
				return err
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
		}
	case http.StatusPartialContent:
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return err
		}
	default:
		return readAPIError(resp)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Sync()
}

func (c *sbsClient) Keep(tag string, names []string) error {
	for len(names) > 0 {
		n := len(names)
		if n > cacheKeepBatch {
			n = cacheKeepBatch
		}
		batch := make([]string, n)
		for i, logical := range names[:n] {
			wire, err := wireObjectName(logical)
			if err != nil {
				return err
			}
			batch[i] = wire
		}
		names = names[n:]
		body, err := json.Marshal(map[string][]string{"names": batch})
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodPost, c.tagURL(tag, "/keep"), bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return readAPIError(resp)
		}
		resp.Body.Close()
	}
	return nil
}

func retryable(err error) bool {
	if err == nil || errors.Is(err, errNotFound) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusBadGateway || se.code == http.StatusTooManyRequests
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "temporarily") ||
		strings.Contains(msg, "broken pipe")
}
