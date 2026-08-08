package deps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // legacy shasum verification only, never for new integrity
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxTarball bounds a downloaded package tarball. A registry entry claiming to be
// gigabytes is a resource attack, not a dependency.
const maxTarball = 64 << 20 // 64 MiB

// client talks to an npm-compatible registry. It is the ONLY thing in the deps
// pipeline that touches the network, and it does so on the daemon's behalf — never
// the guest's — which is the whole reason resolution happens here and not in the
// sandbox (§4.4).
type client struct {
	http     *http.Client
	registry string
}

// packument is the registry's package document, trimmed to what resolution needs.
type packument struct {
	Versions map[string]struct {
		Dist struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
			Shasum    string `json:"shasum"`
		} `json:"dist"`
	} `json:"versions"`
}

// resolve turns name@range into a concrete version + tarball URL + integrity, by
// fetching the packument and picking the highest satisfying version.
func (c *client) resolve(ctx context.Context, name, rng string) (resolution, error) {
	pm, err := c.packument(ctx, name)
	if err != nil {
		return resolution{}, err
	}
	versions := make([]string, 0, len(pm.Versions))
	for v := range pm.Versions {
		versions = append(versions, v)
	}
	best, err := pickVersion(versions, rng)
	if err != nil {
		return resolution{}, fmt.Errorf("%s: %w", name, err)
	}
	dist := pm.Versions[best].Dist
	integrity := dist.Integrity
	if integrity == "" && dist.Shasum != "" {
		// Older packages predate SRI and carry only a hex sha1. Represent it in the
		// same "algo-value" shape so verifyIntegrity has one code path.
		integrity = "sha1-hex:" + dist.Shasum
	}
	if integrity == "" {
		return resolution{}, fmt.Errorf("%s@%s: registry entry has no integrity", name, best)
	}
	return resolution{Version: best, Integrity: integrity, Tarball: dist.Tarball}, nil
}

// packument fetches and decodes the package document.
func (c *client) packument(ctx context.Context, name string) (packument, error) {
	// registry.npmjs.org accepts a scoped name's slash unescaped; keep it simple.
	url := c.registry + "/" + name
	body, err := c.get(ctx, url, maxTarball)
	if err != nil {
		return packument{}, fmt.Errorf("packument %s: %w", name, err)
	}
	var pm packument
	if err := json.Unmarshal(body, &pm); err != nil {
		return packument{}, fmt.Errorf("packument %s: %w", name, err)
	}
	return pm, nil
}

// fetchTarball downloads a tarball, verifies it against integrity BEFORE reading
// its contents, and returns the package's files with the leading "package/"
// stripped. No install script is ever run — the pipeline reads files out of a
// tar, so the preinstall/postinstall attack surface structurally does not exist.
func (c *client) fetchTarball(ctx context.Context, url, integrity string) (map[string][]byte, error) {
	raw, err := c.get(ctx, url, maxTarball)
	if err != nil {
		return nil, fmt.Errorf("tarball %s: %w", url, err)
	}
	if err := verifyIntegrity(raw, integrity); err != nil {
		return nil, fmt.Errorf("tarball %s: %w", url, err)
	}
	return extractTarball(raw)
}

// get issues a bounded GET and returns the body, capping the read so a hostile
// registry cannot stream unbounded data at the daemon.
func (c *client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// verifyIntegrity checks raw against a Subresource-Integrity string
// ("sha512-<base64>") or the legacy "sha1-hex:<hex>". A mismatch is the single
// most important check in the pipeline: it is what makes a cached, pinned
// resolution mean the same bytes every time.
func verifyIntegrity(raw []byte, integrity string) error {
	switch {
	case strings.HasPrefix(integrity, "sha512-"):
		want, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(integrity, "sha512-"))
		if err != nil {
			return fmt.Errorf("bad integrity encoding: %w", err)
		}
		sum := sha512.Sum512(raw)
		if !bytes.Equal(sum[:], want) {
			return fmt.Errorf("integrity mismatch (sha512)")
		}
		return nil
	case strings.HasPrefix(integrity, "sha1-hex:"):
		// Legacy shasum path for pre-SRI packages; weaker, but still pins the bytes.
		want, err := hex.DecodeString(strings.TrimPrefix(integrity, "sha1-hex:"))
		if err != nil {
			return fmt.Errorf("bad shasum encoding: %w", err)
		}
		sum := sha1.Sum(raw) //nolint:gosec // pinning legacy bytes, not a security primitive
		if !bytes.Equal(sum[:], want) {
			return fmt.Errorf("integrity mismatch (sha1)")
		}
		return nil
	default:
		return fmt.Errorf("unsupported integrity %q", integrity)
	}
}

// extractTarball reads a gzipped tar into a name→bytes map, stripping the leading
// "package/" component npm tarballs use. Only regular files are kept; symlinks,
// devices, and directories are dropped, and an entry escaping via ".." is refused.
func extractTarball(raw []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close() //nolint:errcheck

	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // no symlinks, no devices — read only regular files
		}
		name := strings.TrimPrefix(hdr.Name, "package/")
		if name == hdr.Name && !strings.HasPrefix(hdr.Name, "./") {
			name = strings.TrimPrefix(name, "./")
		}
		if strings.Contains(name, "..") {
			return nil, fmt.Errorf("unsafe tar entry %q", hdr.Name)
		}
		total += hdr.Size
		if total > maxTarball {
			return nil, fmt.Errorf("package exceeds %d bytes uncompressed", maxTarball)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxTarball))
		if err != nil {
			return nil, fmt.Errorf("tar read %q: %w", name, err)
		}
		files[name] = data
	}
	return files, nil
}
