package mtls

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"spawnery/internal/pki"
)

const MaxCRLResponseSize = 6 << 20
const CRLFetchTimeout = 15 * time.Second

var (
	ErrCRLFetch            = errors.New("mtls: fetch CRL")
	ErrCRLStatus           = errors.New("mtls: unexpected CRL response status")
	ErrCRLResponseTooLarge = errors.New("mtls: CRL response exceeds size limit")
	ErrCRLSourceNotRegular = errors.New("mtls: CRL file source is not a regular file")
)

type CRLSource struct {
	Issuer *x509.Certificate
	URL    string
	Path   string
}

type CRLRefresher struct {
	client      *http.Client
	sources     []CRLSource
	state       *pki.RevocationState
	interval    time.Duration
	fileMu      sync.Mutex
	fileFlights map[string]*crlFileFlight
	fileLoader  func(string) ([]byte, error)
}

type crlFileFlight struct {
	done chan struct{}
	data []byte
	err  error
}

func BuildCRLSources(issuers []*x509.Certificate, paths, urls []string) ([]CRLSource, error) {
	if len(issuers) == 0 || (len(paths) == 0) == (len(urls) == 0) {
		return nil, errors.New("mtls: exactly one complete CRL source channel is required")
	}
	if len(paths) != 0 && len(paths) != len(issuers) || len(urls) != 0 && len(urls) != len(issuers) {
		return nil, errors.New("mtls: CRL sources must match issuers")
	}
	sources := make([]CRLSource, 0, len(paths)+len(urls))
	for index, path := range paths {
		sources = append(sources, CRLSource{Issuer: issuers[index], Path: path})
	}
	for index, rawURL := range urls {
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Host == "" || parsed.User != nil || !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
			return nil, fmt.Errorf("mtls: invalid CRL HTTP URL %q", rawURL)
		}
		sources = append(sources, CRLSource{Issuer: issuers[index], URL: parsed.String()})
	}
	return sources, nil
}

func NewCRLRefresher(client *http.Client, sources []CRLSource, state *pki.RevocationState, interval time.Duration) *CRLRefresher {
	if client == nil {
		client = http.DefaultClient
	}
	return &CRLRefresher{
		client:      client,
		sources:     append([]CRLSource(nil), sources...),
		state:       state,
		interval:    interval,
		fileFlights: make(map[string]*crlFileFlight),
		fileLoader:  readCRLFile,
	}
}

func (r *CRLRefresher) Refresh(ctx context.Context) error {
	if r == nil || r.state == nil || r.client == nil || len(r.sources) == 0 {
		return errors.New("mtls: invalid CRL refresher configuration")
	}
	var failures []error
	for _, source := range r.sources {
		if source.Issuer == nil || (source.URL == "") == (source.Path == "") {
			failures = append(failures, errors.New("mtls: invalid CRL source"))
			continue
		}
		if err := r.refreshSource(ctx, source); err != nil {
			location := source.URL
			if location == "" {
				location = source.Path
			}
			failures = append(failures, fmt.Errorf("issuer %s from %s: %w", source.Issuer.SerialNumber.Text(16), location, err))
		}
	}
	return errors.Join(failures...)
}

func (r *CRLRefresher) refreshSource(ctx context.Context, source CRLSource) error {
	if source.Path != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, CRLFetchTimeout)
		defer cancel()
		body, err := r.loadFile(fetchCtx, source.Path)
		if err != nil {
			return err
		}
		if err := r.state.ApplyPEM(body); err != nil {
			return fmt.Errorf("apply CRL: %w", err)
		}
		return nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, CRLFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, source.URL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCRLFetch, err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCRLFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", ErrCRLStatus, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxCRLResponseSize+1))
	if err != nil {
		return fmt.Errorf("%w: read response: %v", ErrCRLFetch, err)
	}
	if len(body) > MaxCRLResponseSize {
		return ErrCRLResponseTooLarge
	}
	if err := r.state.ApplyPEM(body); err != nil {
		return fmt.Errorf("apply CRL: %w", err)
	}
	return nil
}

func (r *CRLRefresher) loadFile(ctx context.Context, path string) ([]byte, error) {
	r.fileMu.Lock()
	flight := r.fileFlights[path]
	if flight == nil {
		flight = &crlFileFlight{done: make(chan struct{})}
		r.fileFlights[path] = flight
		loader := r.fileLoader
		go func() {
			flight.data, flight.err = loader(path)
			close(flight.done)
			r.fileMu.Lock()
			if r.fileFlights[path] == flight {
				delete(r.fileFlights, path)
			}
			r.fileMu.Unlock()
		}()
	}
	r.fileMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return flight.data, flight.err
	}
}

func readCRLFile(path string) ([]byte, error) {
	file, err := openCRLFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, MaxCRLResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read file: %v", ErrCRLFetch, err)
	}
	if len(body) > MaxCRLResponseSize {
		return nil, ErrCRLResponseTooLarge
	}
	return body, nil
}

func openCRLFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s", ErrCRLSourceNotRegular, path)
		}
		return nil, fmt.Errorf("%w: open file: %v", ErrCRLFetch, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: open file descriptor", ErrCRLFetch)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: stat file: %v", ErrCRLFetch, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s", ErrCRLSourceNotRegular, path)
	}
	return file, nil
}

func (r *CRLRefresher) Run(ctx context.Context) {
	r.run(ctx, r.interval)
}

func (r *CRLRefresher) RunEvery(ctx context.Context, interval time.Duration) {
	r.run(ctx, interval)
}

func (r *CRLRefresher) run(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Refresh(ctx); err != nil {
				log.Printf("mtls: CRL refresh failed: %v", err)
			}
		}
	}
}
