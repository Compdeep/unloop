package agent

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTheWorkPageTheSiteLinksTo(t *testing.T) {
	const objective = "HEEY4mnKL1PTt4HLoLmutNzPhnowADak47YKbSBPYTsu"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/objectives/"+objective, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"objective":{"status":"open","claimDeadline":"4102444800"},"colony":{"slug":"protein-design"},
"spec":{"verified":true,"body":{"title":"Binders <script>alert(1)</script>"}}}`))
	})
	mux.HandleFunc("/api/colonies", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"slug":"protein-design"}]`))
	})
	site := httptest.NewServer(mux)
	defer site.Close()

	var following []string
	follow := func(c string, on bool) error { following = append(following, c); return nil }
	h := workHandler(newPlatform(site.URL), follow, func() []string { return following }, "127.0.0.1:7777")
	do := func(method, path string, header map[string]string, form url.Values) *httptest.ResponseRecorder {
		var body *strings.Reader = strings.NewReader("")
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, "http://127.0.0.1:7777"+path, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	page := do("GET", "/unloop/work?objective="+objective, nil, nil).Body.String()
	for _, want := range []string{"Binders &lt;script&gt;", "Follow protein-design", "Read objective " + objective, "claims close 2100-01-01", `href="/"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, "<script>alert") {
		t.Error("the objective's title is not escaped")
	}
	if c := do("GET", "/unloop/work?objective=nonsense", nil, nil).Code; c != 400 {
		t.Errorf("no objective: %d", c)
	}

	form := url.Values{"colony": {"protein-design"}, "objective": {objective}}
	if c := do("POST", "/unloop/work/follow", map[string]string{"Sec-Fetch-Site": "cross-site"}, form).Code; c != 403 || len(following) != 0 {
		t.Errorf("a form from elsewhere: %d, following %v", c, following)
	}
	if c := do("POST", "/unloop/work/follow", map[string]string{"Sec-Fetch-Site": "same-origin"}, url.Values{"colony": {"nowhere"}}).Code; c != 400 || len(following) != 0 {
		t.Errorf("no such colony: %d, following %v", c, following)
	}
	if rec := do("POST", "/unloop/work/follow", map[string]string{"Sec-Fetch-Site": "same-origin"}, form); rec.Code != 303 || len(following) != 1 {
		t.Errorf("followed: %d, following %v", rec.Code, following)
	}
	if page := do("GET", "/unloop/work?objective="+objective, nil, nil).Body.String(); !strings.Contains(page, "Following protein-design") {
		t.Error("the page does not say it follows")
	}

	r := httptest.NewRequest("GET", "http://rebound.example:7777/unloop/work?objective="+objective, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 421 {
		t.Errorf("a rebound name: %d", rec.Code)
	}
}
