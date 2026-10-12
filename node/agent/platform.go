package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// platform is the site the agent browses through: its index for searching
// and its archive for reading. Anybody may run one; the person chooses which.
// What the agent acts on it reads from the chain, never from here.
type platform struct {
	base string
	http *http.Client
}

func newPlatform(base string) *platform {
	return &platform{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 20 * time.Second}}
}

// get reads a JSON answer into out.
func (p *platform) get(ctx context.Context, path string, query url.Values, out any) error {
	u := p.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	res, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", path, e.Error)
		}
		return fmt.Errorf("%s: the platform answered %s", path, res.Status)
	}
	return json.Unmarshal(body, out)
}

// post sends a JSON body; the status, and the answer read into out when it is 2xx.
func (p *platform) post(ctx context.Context, path string, body any, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := p.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(answer, &e)
		return res.StatusCode, fmt.Errorf("%s: %s %s", path, res.Status, e.Error)
	}
	if out != nil {
		return res.StatusCode, json.Unmarshal(answer, out)
	}
	return res.StatusCode, nil
}
