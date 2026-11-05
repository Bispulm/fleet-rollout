package api_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const testAdminToken = "test-admin-credential-value"

type harness struct {
	t      *testing.T
	base   string
	signer ed25519.PrivateKey
}

func newHarness(t *testing.T) harness {
	t.Helper()
	return newHarnessWith(t, nil)
}

// newHarnessWithLimit sets the largest artifact the server accepts.
func newHarnessWithLimit(t *testing.T, limit int64) harness {
	t.Helper()
	return newHarnessWith(t, func(o *api.Options) { o.MaxArtifactBytes = limit })
}

// newHarnessWith lets a test change the server's options.
func newHarnessWith(t *testing.T, edit func(*api.Options)) harness {
	t.Helper()
	t.Parallel()
	pool := testdb.New(t)
	signer := signingtest.NewKey(t)
	artifacts, err := artifactstore.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	opts := api.Options{
		Logger:     slog.New(slog.DiscardHandler),
		Ready:      alwaysReady,
		Store:      store.New(pool),
		AdminToken: testAdminToken,
		Trust:      signingtest.Trust(t, signer),
		Artifacts:  artifacts,
	}
	if edit != nil {
		edit(&opts)
	}
	srv := api.New(opts)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return harness{t: t, base: ts.URL, signer: signer}
}

// on returns a harness that reports failures to t. Subtests must use it, because
// Fatalf has to be called on the test running in the current goroutine.
func (h harness) on(t *testing.T) harness {
	t.Helper()
	h.t = t
	return h
}

// do sends a request with the admin token and returns the status and raw body.
func (h harness) do(method, path string, body any) (int, []byte) {
	h.t.Helper()
	return h.doWithAuth("Bearer "+testAdminToken, method, path, body)
}

func (h harness) doWithAuth(authorization, method, path string, body any) (int, []byte) {
	h.t.Helper()
	var reader io.Reader = http.NoBody
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.base+path, reader)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, raw
}

// expect sends a request, requires a status, and decodes the body into out.
func (h harness) expect(wantStatus int, method, path string, body, out any) {
	h.t.Helper()
	status, raw := h.do(method, path, body)
	if status != wantStatus {
		h.t.Fatalf("%s %s: status = %d, want %d; body: %s", method, path, status, wantStatus, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("decode %q: %v", raw, err)
		}
	}
}

func decode(t *testing.T, raw []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
}

func (h harness) problem(wantStatus int, method, path string, body any) apiv1.Problem {
	h.t.Helper()
	var p apiv1.Problem
	h.expect(wantStatus, method, path, body, &p)
	if p.Status != wantStatus {
		h.t.Fatalf("problem status = %d, want %d", p.Status, wantStatus)
	}
	return p
}

func (h harness) createFleet(name string) apiv1.Fleet {
	h.t.Helper()
	var fleet apiv1.Fleet
	h.expect(http.StatusCreated, http.MethodPost, "/v1/fleets", apiv1.CreateFleetRequest{Name: name}, &fleet)
	return fleet
}

func deviceSpecs(n int) []apiv1.DeviceSpec {
	specs := make([]apiv1.DeviceSpec, n)
	for i := range specs {
		specs[i] = apiv1.DeviceSpec{ID: fmt.Sprintf("sim-%05d", i), Platform: "linux/arm64"}
	}
	return specs
}

func (h harness) registerDevices(fleet string, n int) []apiv1.Device {
	h.t.Helper()
	var out apiv1.DeviceList
	h.expect(http.StatusCreated, http.MethodPost, "/v1/fleets/"+fleet+"/devices",
		apiv1.RegisterDevicesRequest{Devices: deviceSpecs(n)}, &out)
	return out.Devices
}

func TestAdminRoutesRequireTheAdminToken(t *testing.T) {
	h := newHarness(t)
	for name, authorization := range map[string]string{
		"missing": "",
		"wrong":   "Bearer not-the-token",
		"scheme":  "Basic " + testAdminToken,
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := h.on(t).doWithAuth(authorization, http.MethodGet, "/v1/fleets/lab-a", nil)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, body: %s", status, raw)
			}
		})
	}
}

func TestDeviceTokensAreNotAdminCredentials(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	token := h.registerDevices("lab-a", 1)[0].Token
	if status, _ := h.doWithAuth("Bearer "+token, http.MethodGet, "/v1/fleets/lab-a", nil); status != http.StatusUnauthorized {
		t.Fatalf("device token on an admin route: status = %d", status)
	}
}

func TestHealthRoutesNeedNoToken(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.doWithAuth("", http.MethodGet, "/healthz", nil); status != http.StatusOK {
		t.Fatalf("healthz status = %d", status)
	}
}

func TestCreateAndGetFleet(t *testing.T) {
	h := newHarness(t)
	var created apiv1.Fleet
	h.expect(http.StatusCreated, http.MethodPost, "/v1/fleets",
		apiv1.CreateFleetRequest{Name: "lab-a", Labels: map[string]string{"site": "lab"}}, &created)
	if created.ID == "" || created.Labels["site"] != "lab" {
		t.Fatalf("created = %+v", created)
	}

	var got apiv1.Fleet
	h.expect(http.StatusOK, http.MethodGet, "/v1/fleets/lab-a", nil, &got)
	if got.ID != created.ID {
		t.Fatalf("got = %+v", got)
	}

	if p := h.problem(http.StatusConflict, http.MethodPost, "/v1/fleets", apiv1.CreateFleetRequest{Name: "lab-a"}); p.Code != apiv1.CodeConflict {
		t.Fatalf("duplicate code = %q", p.Code)
	}
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/fleets/missing", nil)
}

func TestRequestValidation(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	cases := []struct {
		name   string
		status int
		path   string
		body   any
	}{
		{"malformed JSON", http.StatusBadRequest, "/v1/fleets", `{"name":`},
		{"unknown field", http.StatusBadRequest, "/v1/fleets", `{"name":"x","surprise":1}`},
		{"trailing data", http.StatusBadRequest, "/v1/fleets", `{"name":"x"} {"name":"y"}`},
		{"bad fleet name", http.StatusUnprocessableEntity, "/v1/fleets", apiv1.CreateFleetRequest{Name: "Not Valid"}},
		{"bad fleet labels", http.StatusUnprocessableEntity, "/v1/fleets", apiv1.CreateFleetRequest{Name: "ok", Labels: map[string]string{"bad key": "x"}}},
		{"no devices", http.StatusUnprocessableEntity, "/v1/fleets/lab-a/devices", apiv1.RegisterDevicesRequest{}},
		{"bad device id", http.StatusUnprocessableEntity, "/v1/fleets/lab-a/devices", apiv1.RegisterDevicesRequest{
			Devices: []apiv1.DeviceSpec{{ID: "bad id", Platform: "linux/arm64"}}}},
		{"bad platform", http.StatusUnprocessableEntity, "/v1/fleets/lab-a/devices", apiv1.RegisterDevicesRequest{
			Devices: []apiv1.DeviceSpec{{ID: "ok", Platform: "linux"}}}},
		{"negative release", http.StatusUnprocessableEntity, "/v1/fleets/lab-a/devices", apiv1.RegisterDevicesRequest{
			Devices: []apiv1.DeviceSpec{{ID: "ok", Platform: "linux/arm64", InstalledRelease: -1}}}},
		{"duplicate in batch", http.StatusUnprocessableEntity, "/v1/fleets/lab-a/devices", apiv1.RegisterDevicesRequest{
			Devices: []apiv1.DeviceSpec{{ID: "ok", Platform: "linux/arm64"}, {ID: "ok", Platform: "linux/arm64"}}}},
		{"batch too large", http.StatusUnprocessableEntity, "/v1/fleets/lab-a/devices", apiv1.RegisterDevicesRequest{Devices: deviceSpecs(1001)}},
		{"body too large", http.StatusRequestEntityTooLarge, "/v1/fleets", `{"name":"` + strings.Repeat("x", 5<<20) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.on(t).problem(tc.status, http.MethodPost, tc.path, tc.body)
		})
	}
}

func TestRegisterDevicesReturnsTokensOnlyOnce(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	devices := h.registerDevices("lab-a", 3)
	if len(devices) != 3 {
		t.Fatalf("registered %d devices", len(devices))
	}
	seen := map[string]bool{}
	for _, d := range devices {
		if d.Token == "" || seen[d.Token] {
			t.Fatalf("device %s has a missing or repeated token", d.ID)
		}
		seen[d.Token] = true
	}

	status, raw := h.do(http.MethodGet, "/v1/fleets/lab-a/devices", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if bytes.Contains(raw, []byte(`"token"`)) || bytes.Contains(raw, []byte("credential")) {
		t.Fatalf("listing exposes credentials: %s", raw)
	}
}

func TestRegisterDevicesConflictIsAllOrNothing(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	h.registerDevices("lab-a", 1)
	p := h.problem(http.StatusConflict, http.MethodPost, "/v1/fleets/lab-a/devices", apiv1.RegisterDevicesRequest{Devices: deviceSpecs(3)})
	if !strings.Contains(p.Detail, "sim-00000") {
		t.Fatalf("detail = %q, want it to name the existing device", p.Detail)
	}
	var list apiv1.DeviceList
	h.expect(http.StatusOK, http.MethodGet, "/v1/fleets/lab-a/devices", nil, &list)
	if len(list.Devices) != 1 {
		t.Fatalf("fleet has %d devices after a rejected batch", len(list.Devices))
	}
}

func TestListDevicesPagination(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	h.registerDevices("lab-a", 5)

	var page apiv1.DeviceList
	h.expect(http.StatusOK, http.MethodGet, "/v1/fleets/lab-a/devices?limit=2", nil, &page)
	if len(page.Devices) != 2 || page.NextAfter != "sim-00001" {
		t.Fatalf("first page = %+v", page)
	}
	h.expect(http.StatusOK, http.MethodGet, "/v1/fleets/lab-a/devices?limit=2&after="+page.NextAfter, nil, &page)
	if page.Devices[0].ID != "sim-00002" {
		t.Fatalf("second page = %+v", page)
	}
	var last apiv1.DeviceList
	h.expect(http.StatusOK, http.MethodGet, "/v1/fleets/lab-a/devices?limit=10&after=sim-00003", nil, &last)
	if len(last.Devices) != 1 || last.NextAfter != "" {
		t.Fatalf("last page = %+v", last)
	}
	for _, q := range []string{"limit=0", "limit=-1", "limit=x", "limit=1001"} {
		h.problem(http.StatusBadRequest, http.MethodGet, "/v1/fleets/lab-a/devices?"+q, nil)
	}
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/fleets/missing/devices", nil)
}

func TestUpdateDeviceStatus(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	h.registerDevices("lab-a", 1)

	var updated apiv1.Device
	h.expect(http.StatusOK, http.MethodPatch, "/v1/devices/sim-00000", `{"status":"excluded"}`, &updated)
	if updated.Status != "excluded" {
		t.Fatalf("updated = %+v", updated)
	}
	h.problem(http.StatusUnprocessableEntity, http.MethodPatch, "/v1/devices/sim-00000", `{"status":"gone"}`)
	h.problem(http.StatusNotFound, http.MethodPatch, "/v1/devices/missing", `{"status":"active"}`)
}
