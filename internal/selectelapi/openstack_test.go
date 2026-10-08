package selectelapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

const (
	testToken  = "token-1"
	testRegion = "ru-9"
)

// fakeOpenStack — Keystone, Nova, Neutron и Glance на одном httptest-сервере
// (аналог httptest для настоящего gophercloud). Ответы задаются по
// "METHOD /path", все запросы записываются.
type fakeOpenStack struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests []recorded
	authBody map[string]any
}

type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   map[string]any
}

func newFakeOpenStack(t *testing.T) *fakeOpenStack {
	t.Helper()

	f := &fakeOpenStack{t: t, handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenStack) handle(pattern string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[pattern] = h
}

func (f *fakeOpenStack) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s %s: invalid json body: %v", r.Method, r.URL.Path, err)
		}
	}

	if r.Method == http.MethodPost && r.URL.Path == "/identity/v3/auth/tokens" {
		f.mu.Lock()
		f.authBody = body
		h := f.handlers["POST /identity/v3/auth/tokens"]
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		f.issueToken(w)
		return
	}

	// документы версий для неверсионированных endpoint-ов Neutron и Glance,
	// как их отдаёт настоящий OpenStack
	if r.Method == http.MethodGet && (r.URL.Path == "/network/" || r.URL.Path == "/image/") {
		id := map[string]string{"/network/": "v2.0", "/image/": "v2.16"}[r.URL.Path]
		writeJSON(w, http.StatusOK, map[string]any{"versions": []any{map[string]any{"id": id, "status": "CURRENT"}}})
		return
	}

	if got := r.Header.Get("X-Auth-Token"); got != testToken {
		f.t.Errorf("%s %s: X-Auth-Token = %q", r.Method, r.URL.Path, got)
	}

	f.mu.Lock()
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
	h := f.handlers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()

	if h == nil {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h(w, r)
}

func (f *fakeOpenStack) issueToken(w http.ResponseWriter) {
	endpoint := func(region, path string) map[string]any {
		return map[string]any{"interface": "public", "region": region, "region_id": region, "url": f.srv.URL + path}
	}
	service := func(kind, path string) map[string]any {
		return map[string]any{
			"type": kind,
			"endpoints": []any{
				// другой пул — должен быть проигнорирован
				endpoint("ru-7", "/wrong-region"+path),
				endpoint(testRegion, path),
			},
		}
	}

	w.Header().Set("X-Subject-Token", testToken)
	writeJSON(w, http.StatusCreated, map[string]any{"token": map[string]any{
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"catalog": []any{
			service("compute", "/compute/v2.1/"),
			service("network", "/network/"),
			service("image", "/image/"),
		},
	}})
}

// requestsTo — записанные запросы с этим методом и путём.
func (f *fakeOpenStack) requestsTo(method, path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()

	var result []recorded
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			result = append(result, r)
		}
	}
	return result
}

func (f *fakeOpenStack) client(t *testing.T) *openStack {
	t.Helper()

	c, err := newOpenStack(context.Background(), Auth{
		AuthURL:   f.srv.URL + "/identity/v3",
		AccountID: "123456",
		Username:  "fleeting",
		Password:  "secret",
		ProjectID: "project-1",
		Region:    testRegion,
	})
	if err != nil {
		t.Fatalf("newOpenStack() = %v", err)
	}
	c.pollInterval = time.Millisecond
	return c
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func respond(code int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, code, v) }
}

func noContent(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

// dig достаёт значение по пути ключей из разобранного JSON.
func dig(v any, keys ...any) any {
	for _, k := range keys {
		switch key := k.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[key]
		case int:
			s, _ := v.([]any)
			if key >= len(s) {
				return nil
			}
			v = s[key]
		}
	}
	return v
}

func server(id, status string, extra map[string]any) map[string]any {
	s := map[string]any{"id": id, "name": "runner-" + id, "status": status, "metadata": map[string]any{"fleeting-group": "runner"}}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

func TestAuthRequest(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /compute/v2.1/servers/srv-1", respond(200, map[string]any{"server": server("srv-1", StatusActive, nil)}))

	c := f.client(t)
	if _, err := c.GetInstance(context.Background(), "srv-1"); err != nil {
		t.Fatalf("GetInstance() = %v", err)
	}

	user := dig(f.authBody, "auth", "identity", "password", "user")
	want := map[string]any{"name": "fleeting", "password": "secret", "domain": map[string]any{"name": "123456"}}
	if !reflect.DeepEqual(user, want) {
		t.Fatalf("auth user = %v, want %v", user, want)
	}
	if got := dig(f.authBody, "auth", "scope", "project", "id"); got != "project-1" {
		t.Fatalf("auth scope project = %v", got)
	}

	// запрос ушёл на endpoint своего пула и с микроверсией
	req := f.requestsTo("GET", "/compute/v2.1/servers/srv-1")
	if len(req) != 1 {
		t.Fatalf("requests to the ru-9 compute endpoint = %d", len(req))
	}
	if got := req[0].Header.Get("X-OpenStack-Nova-API-Version"); got != computeMicroversion {
		t.Fatalf("microversion = %q", got)
	}
}

func TestAuthFailure(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("POST /identity/v3/auth/tokens", respond(401, map[string]any{"error": map[string]any{"message": "invalid credentials"}}))

	auth := Auth{AuthURL: f.srv.URL + "/identity/v3", AccountID: "1", Username: "u", Password: "wrong", ProjectID: "p", Region: testRegion}
	_, err := newOpenStack(context.Background(), auth)
	if err == nil || !strings.Contains(err.Error(), "could not authenticate") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("newOpenStack() = %v, want a 401 auth error", err)
	}
}

func TestUnknownRegion(t *testing.T) {
	f := newFakeOpenStack(t)

	auth := Auth{AuthURL: f.srv.URL + "/identity/v3", AccountID: "1", Username: "u", Password: "p", ProjectID: "p", Region: "kz-1"}
	_, err := newOpenStack(context.Background(), auth)
	if err == nil || !strings.Contains(err.Error(), "could not find compute endpoint in region kz-1") {
		t.Fatalf("newOpenStack() = %v, want a missing endpoint error", err)
	}
}

func TestListInstancesPaginates(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("marker") == "" {
			writeJSON(w, 200, map[string]any{
				"servers": []any{server("a", StatusActive, nil)},
				"servers_links": []any{map[string]any{
					"rel":  "next",
					"href": f.srv.URL + "/compute/v2.1/servers/detail?marker=a&name=%5Erunner-",
				}},
			})
			return
		}
		writeJSON(w, 200, map[string]any{"servers": []any{server("b", StatusBuild, nil)}})
	})

	c := f.client(t)
	c.pageSize = 1

	list, err := c.ListInstances(context.Background(), "runner-")
	if err != nil {
		t.Fatalf("ListInstances() = %v", err)
	}
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" || list[1].Status != StatusBuild {
		t.Fatalf("ListInstances() = %+v", list)
	}
	if list[0].Metadata["fleeting-group"] != "runner" {
		t.Fatalf("metadata = %v", list[0].Metadata)
	}

	first := f.requestsTo("GET", "/compute/v2.1/servers/detail")[0]
	// name в Nova — регулярное выражение, ищем по началу имени
	if got := first.Query.Get("name"); got != "^runner-" {
		t.Fatalf("name filter = %q", got)
	}
	if got := first.Query.Get("limit"); got != "1" {
		t.Fatalf("limit = %q", got)
	}
}

func TestListInstancesError(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /compute/v2.1/servers/detail", respond(500, map[string]any{"computeFault": map[string]any{"message": "boom"}}))

	if _, err := f.client(t).ListInstances(context.Background(), "runner-"); err == nil {
		t.Fatal("ListInstances() = nil error")
	}
}

func TestFromServerAddresses(t *testing.T) {
	addr := func(ip string, version int, kind string) map[string]any {
		return map[string]any{"addr": ip, "version": version, "OS-EXT-IPS:type": kind}
	}

	tests := []struct {
		name               string
		addresses          map[string]any
		internal, external string
	}{
		{
			name:      "private with floating",
			addresses: map[string]any{"net": []any{addr("fe80::1", 6, "fixed"), addr("192.168.0.5", 4, "fixed"), addr("203.0.113.7", 4, "floating")}},
			internal:  "192.168.0.5", external: "203.0.113.7",
		},
		{
			name:      "private only",
			addresses: map[string]any{"net": []any{addr("10.0.0.5", 4, "fixed")}},
			internal:  "10.0.0.5",
		},
		{
			name:      "direct public subnet",
			addresses: map[string]any{"public": []any{addr("203.0.113.9", 4, "fixed")}},
			internal:  "203.0.113.9", external: "203.0.113.9",
		},
		{name: "no addresses yet", addresses: map[string]any{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeOpenStack(t)
			f.handle("GET /compute/v2.1/servers/srv-1", respond(200, map[string]any{
				"server": server("srv-1", StatusActive, map[string]any{"addresses": tt.addresses}),
			}))

			got, err := f.client(t).GetInstance(context.Background(), "srv-1")
			if err != nil {
				t.Fatalf("GetInstance() = %v", err)
			}
			if got.InternalIP != tt.internal || got.ExternalIP != tt.external {
				t.Fatalf("addresses = %q / %q, want %q / %q", got.InternalIP, got.ExternalIP, tt.internal, tt.external)
			}
		})
	}
}

func TestGetInstanceNotFound(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /compute/v2.1/servers/gone", respond(404, map[string]any{"itemNotFound": map[string]any{"message": "not found"}}))

	if _, err := f.client(t).GetInstance(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetInstance() = %v, want ErrNotFound", err)
	}
}

func createRequest() CreateInstanceRequest {
	return CreateInstanceRequest{
		Name:              "runner-0000abcd",
		Metadata:          map[string]string{"fleeting-group": "runner"},
		Tags:              []string{"preemptible"},
		AvailabilityZone:  "ru-9a",
		FlavorID:          "flavor-1",
		ImageID:           "image-1",
		VolumeType:        "universal.ru-9a",
		DiskSizeGB:        30,
		NetworkID:         "net-1",
		SubnetID:          "subnet-1",
		SecurityGroupIDs:  []string{"sg-1"},
		FloatingNetworkID: "ext-net",
		KeyName:           "fleeting-runner-1",
		UserData:          "#cloud-config\n",
		Group:             "runner",
	}
}

func handleCreate(f *fakeOpenStack) {
	f.handle("POST /network/v2.0/ports", respond(201, map[string]any{"port": map[string]any{"id": "port-1"}}))
	f.handle("POST /network/v2.0/floatingips", respond(201, map[string]any{"floatingip": map[string]any{"id": "fip-1", "floating_ip_address": "203.0.113.7"}}))
	f.handle("POST /compute/v2.1/servers", respond(202, map[string]any{"server": map[string]any{"id": "srv-1"}}))
}

func TestCreateInstance(t *testing.T) {
	f := newFakeOpenStack(t)
	handleCreate(f)

	op, err := f.client(t).CreateInstance(context.Background(), createRequest())
	if err != nil {
		t.Fatalf("CreateInstance() = %v", err)
	}
	if op.InstanceID() != "srv-1" {
		t.Fatalf("InstanceID() = %q", op.InstanceID())
	}

	port := f.requestsTo("POST", "/network/v2.0/ports")[0].Body["port"]
	wantPort := map[string]any{
		"network_id":      "net-1",
		"name":            "runner-0000abcd",
		"description":     "fleeting-group=runner",
		"fixed_ips":       []any{map[string]any{"subnet_id": "subnet-1"}},
		"security_groups": []any{"sg-1"},
	}
	if !reflect.DeepEqual(port, wantPort) {
		t.Fatalf("port request = %v\nwant %v", port, wantPort)
	}

	fip := f.requestsTo("POST", "/network/v2.0/floatingips")[0].Body["floatingip"]
	wantFIP := map[string]any{"floating_network_id": "ext-net", "port_id": "port-1", "description": "fleeting-group=runner"}
	if !reflect.DeepEqual(fip, wantFIP) {
		t.Fatalf("floating ip request = %v, want %v", fip, wantFIP)
	}

	create := f.requestsTo("POST", "/compute/v2.1/servers")[0]
	if got := create.Header.Get("X-OpenStack-Nova-API-Version"); got != "2.72" {
		t.Fatalf("microversion = %q, want 2.72 for preemptible tag", got)
	}
	s := create.Body["server"].(map[string]any)

	for key, want := range map[string]any{
		"name":              "runner-0000abcd",
		"flavorRef":         "flavor-1",
		"availability_zone": "ru-9a",
		"key_name":          "fleeting-runner-1",
		"metadata":          map[string]any{"fleeting-group": "runner"},
		"tags":              []any{"preemptible"},
		"networks":          []any{map[string]any{"port": "port-1"}},
		"user_data":         base64.StdEncoding.EncodeToString([]byte("#cloud-config\n")),
	} {
		if !reflect.DeepEqual(s[key], want) {
			t.Errorf("server.%s = %#v, want %#v", key, s[key], want)
		}
	}

	wantBDM := []any{map[string]any{
		"source_type":           "image",
		"uuid":                  "image-1",
		"destination_type":      "volume",
		"volume_size":           float64(30),
		"volume_type":           "universal.ru-9a",
		"boot_index":            float64(0),
		"delete_on_termination": true,
	}}
	if !reflect.DeepEqual(s["block_device_mapping_v2"], wantBDM) {
		t.Errorf("block_device_mapping_v2 = %#v\nwant %#v", s["block_device_mapping_v2"], wantBDM)
	}
	// образ — только в BDM, иначе Nova загрузится с локального диска флейвора
	if s["imageRef"] != "" && s["imageRef"] != nil {
		t.Errorf("imageRef = %v, want empty", s["imageRef"])
	}
	for _, key := range []string{"security_groups", "adminPass"} {
		if _, ok := s[key]; ok {
			t.Errorf("server.%s is set, want it omitted", key)
		}
	}
}

// Без floating IP и без ключа — ни адреса, ни key_name, ни user_data.
func TestCreateInstanceMinimal(t *testing.T) {
	f := newFakeOpenStack(t)
	handleCreate(f)

	req := createRequest()
	req.SubnetID, req.SecurityGroupIDs, req.FloatingNetworkID, req.KeyName, req.UserData = "", nil, "", "", ""
	if _, err := f.client(t).CreateInstance(context.Background(), req); err != nil {
		t.Fatalf("CreateInstance() = %v", err)
	}

	if n := len(f.requestsTo("POST", "/network/v2.0/floatingips")); n != 0 {
		t.Fatalf("floating ip requests = %d, want 0", n)
	}
	port := f.requestsTo("POST", "/network/v2.0/ports")[0].Body["port"].(map[string]any)
	for _, key := range []string{"fixed_ips", "security_groups"} {
		if _, ok := port[key]; ok {
			t.Errorf("port.%s is set, want it omitted", key)
		}
	}
	s := f.requestsTo("POST", "/compute/v2.1/servers")[0].Body["server"].(map[string]any)
	for _, key := range []string{"key_name", "user_data"} {
		if _, ok := s[key]; ok {
			t.Errorf("server.%s is set, want it omitted", key)
		}
	}
}

// Сервер не принят — порт и адрес не должны остаться биллиться.
func TestCreateInstanceServerRejectedCleansUp(t *testing.T) {
	f := newFakeOpenStack(t)
	handleCreate(f)
	f.handle("POST /compute/v2.1/servers", respond(403, map[string]any{"forbidden": map[string]any{
		"code": 403, "message": "Quota exceeded for cores: Requested 80, but already used 0 of 20 cores",
	}}))
	f.handle("GET /network/v2.0/floatingips", respond(200, map[string]any{"floatingips": []any{}}))
	f.handle("DELETE /network/v2.0/floatingips/fip-1", noContent)
	f.handle("DELETE /network/v2.0/ports/port-1", noContent)

	_, err := f.client(t).CreateInstance(context.Background(), createRequest())
	if !errors.Is(err, ErrResourceExhausted) {
		t.Fatalf("CreateInstance() = %v, want ErrResourceExhausted", err)
	}
	if len(f.requestsTo("DELETE", "/network/v2.0/floatingips/fip-1")) != 1 || len(f.requestsTo("DELETE", "/network/v2.0/ports/port-1")) != 1 {
		t.Fatal("port or floating ip left behind after the server was rejected")
	}
}

func TestCreateInstanceFloatingIPFailsCleansUpPort(t *testing.T) {
	f := newFakeOpenStack(t)
	handleCreate(f)
	f.handle("POST /network/v2.0/floatingips", respond(409, map[string]any{"NeutronError": map[string]any{
		"type": "OverQuota", "message": "Quota exceeded for resources: ['floatingip'].",
	}}))
	f.handle("GET /network/v2.0/floatingips", respond(200, map[string]any{"floatingips": []any{}}))
	f.handle("DELETE /network/v2.0/ports/port-1", noContent)

	_, err := f.client(t).CreateInstance(context.Background(), createRequest())
	if !errors.Is(err, ErrResourceExhausted) || !strings.Contains(err.Error(), "floating ip") {
		t.Fatalf("CreateInstance() = %v, want a floating ip quota error", err)
	}
	if len(f.requestsTo("DELETE", "/network/v2.0/ports/port-1")) != 1 {
		t.Fatal("port left behind after the floating ip failed")
	}
	if len(f.requestsTo("POST", "/compute/v2.1/servers")) != 0 {
		t.Fatal("server created without its floating ip")
	}
}

// Если и уборка не удалась, это должно быть видно в ошибке.
func TestCreateInstanceCleanupErrorIsReported(t *testing.T) {
	f := newFakeOpenStack(t)
	handleCreate(f)
	f.handle("POST /compute/v2.1/servers", respond(400, map[string]any{"badRequest": map[string]any{"message": "invalid flavor"}}))
	f.handle("DELETE /network/v2.0/floatingips/fip-1", noContent)
	f.handle("DELETE /network/v2.0/ports/port-1", respond(500, map[string]any{"NeutronError": map[string]any{"message": "boom"}}))

	_, err := f.client(t).CreateInstance(context.Background(), createRequest())
	if err == nil || !strings.Contains(err.Error(), "invalid flavor") || !strings.Contains(err.Error(), "could not clean up port port-1") {
		t.Fatalf("CreateInstance() = %v, want both errors", err)
	}
}

func TestCreateOperationWait(t *testing.T) {
	statuses := []map[string]any{
		server("srv-1", StatusBuild, nil),
		server("srv-1", StatusBuild, nil),
		server("srv-1", StatusActive, nil),
	}

	f := newFakeOpenStack(t)
	handleCreate(f)
	var calls int
	f.handle("GET /compute/v2.1/servers/srv-1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"server": statuses[min(calls, len(statuses)-1)]})
		calls++
	})

	op, err := f.client(t).CreateInstance(context.Background(), createRequest())
	if err != nil {
		t.Fatalf("CreateInstance() = %v", err)
	}
	instance, err := op.Wait(context.Background())
	if err != nil || instance.Status != StatusActive || calls != 3 {
		t.Fatalf("Wait() = %+v, %v after %d polls", instance, err, calls)
	}
}

func TestCreateOperationWaitErrors(t *testing.T) {
	tests := []struct {
		name          string
		server        map[string]any
		wantExhausted bool
		wantErr       string
	}{
		{
			name: "no valid host",
			server: server("srv-1", StatusError, map[string]any{"fault": map[string]any{
				"code": 500, "message": "No valid host was found. There are not enough hosts available.",
			}}),
			wantExhausted: true,
			wantErr:       "No valid host",
		},
		{
			name: "other fault",
			server: server("srv-1", StatusError, map[string]any{"fault": map[string]any{
				"code": 500, "message": "Build of instance aborted: Volume did not finish being created",
			}}),
			wantErr: "Volume did not finish",
		},
		{
			name:    "unexpected status",
			server:  server("srv-1", StatusShutoff, nil),
			wantErr: "SHUTOFF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeOpenStack(t)
			f.handle("GET /compute/v2.1/servers/srv-1", respond(200, map[string]any{"server": tt.server}))

			op := &createOperation{c: f.client(t), id: "srv-1"}
			_, err := op.Wait(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Wait() = %v, want an error mentioning %q", err, tt.wantErr)
			}
			if errors.Is(err, ErrResourceExhausted) != tt.wantExhausted {
				t.Fatalf("Wait() = %v, exhausted = %v, want %v", err, errors.Is(err, ErrResourceExhausted), tt.wantExhausted)
			}
		})
	}
}

func TestCreateOperationWaitCancelled(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /compute/v2.1/servers/srv-1", respond(200, map[string]any{"server": server("srv-1", StatusBuild, nil)}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	op := &createOperation{c: f.client(t), id: "srv-1"}
	if _, err := op.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() = %v, want the context error", err)
	}
}

func TestDeleteInstance(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /network/v2.0/ports", respond(200, map[string]any{"ports": []any{map[string]any{"id": "port-1", "device_id": "srv-1"}}}))
	f.handle("DELETE /compute/v2.1/servers/srv-1", noContent)
	f.handle("GET /network/v2.0/floatingips", respond(200, map[string]any{"floatingips": []any{map[string]any{"id": "fip-1", "port_id": "port-1"}}}))
	f.handle("DELETE /network/v2.0/floatingips/fip-1", noContent)
	f.handle("DELETE /network/v2.0/ports/port-1", noContent)

	if err := f.client(t).DeleteInstance(context.Background(), "srv-1", "runner"); err != nil {
		t.Fatalf("DeleteInstance() = %v", err)
	}

	list := f.requestsTo("GET", "/network/v2.0/ports")[0]
	if list.Query.Get("device_id") != "srv-1" || list.Query.Get("description") != "fleeting-group=runner" {
		t.Fatalf("ports query = %v", list.Query)
	}
	if q := f.requestsTo("GET", "/network/v2.0/floatingips")[0].Query; q.Get("port_id") != "port-1" {
		t.Fatalf("floating ips query = %v", q)
	}
	for _, path := range []string{"/compute/v2.1/servers/srv-1", "/network/v2.0/floatingips/fip-1", "/network/v2.0/ports/port-1"} {
		if len(f.requestsTo("DELETE", path)) != 1 {
			t.Errorf("DELETE %s not sent", path)
		}
	}
}

// Сервер уже удалён: порты всё равно убираем, а наружу — ErrNotFound.
func TestDeleteInstanceAlreadyGone(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /network/v2.0/ports", respond(200, map[string]any{"ports": []any{map[string]any{"id": "port-1"}}}))
	f.handle("DELETE /compute/v2.1/servers/srv-1", respond(404, map[string]any{"itemNotFound": map[string]any{"message": "gone"}}))
	f.handle("GET /network/v2.0/floatingips", respond(200, map[string]any{"floatingips": []any{}}))
	f.handle("DELETE /network/v2.0/ports/port-1", respond(404, map[string]any{"NeutronError": map[string]any{"message": "gone"}}))

	err := f.client(t).DeleteInstance(context.Background(), "srv-1", "runner")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteInstance() = %v, want ErrNotFound", err)
	}
	if len(f.requestsTo("DELETE", "/network/v2.0/ports/port-1")) != 1 {
		t.Fatal("port was not deleted")
	}
}

func TestCleanupOrphans(t *testing.T) {
	cutoff := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour).Format("2006-01-02T15:04:05Z")
	young := cutoff.Add(time.Minute).Format("2006-01-02T15:04:05Z")

	f := newFakeOpenStack(t)
	f.handle("GET /network/v2.0/ports", respond(200, map[string]any{"ports": []any{
		map[string]any{"id": "orphan", "device_id": "", "created_at": old},
		map[string]any{"id": "young", "device_id": "", "created_at": young},
		map[string]any{"id": "bound", "device_id": "srv-1", "created_at": old},
	}}))
	f.handle("GET /network/v2.0/floatingips", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("port_id") == "orphan" {
			writeJSON(w, 200, map[string]any{"floatingips": []any{map[string]any{"id": "fip-on-orphan", "port_id": "orphan"}}})
			return
		}
		writeJSON(w, 200, map[string]any{"floatingips": []any{
			map[string]any{"id": "fip-free", "port_id": nil, "created_at": old},
			map[string]any{"id": "fip-young", "port_id": nil, "created_at": young},
			map[string]any{"id": "fip-bound", "port_id": "bound", "created_at": old},
		}})
	})
	f.handle("DELETE /network/v2.0/floatingips/fip-on-orphan", noContent)
	f.handle("DELETE /network/v2.0/ports/orphan", noContent)
	f.handle("DELETE /network/v2.0/floatingips/fip-free", noContent)

	deleted, err := f.client(t).CleanupOrphans(context.Background(), "runner", cutoff)
	if err != nil || deleted != 2 {
		t.Fatalf("CleanupOrphans() = %d, %v, want 2", deleted, err)
	}

	if q := f.requestsTo("GET", "/network/v2.0/ports")[0].Query; q.Get("description") != "fleeting-group=runner" || q.Has("device_id") {
		t.Fatalf("ports query = %v", q)
	}
	for _, path := range []string{"/network/v2.0/ports/orphan", "/network/v2.0/floatingips/fip-on-orphan", "/network/v2.0/floatingips/fip-free"} {
		if len(f.requestsTo("DELETE", path)) != 1 {
			t.Errorf("DELETE %s not sent", path)
		}
	}
}

func TestLatestImageByName(t *testing.T) {
	image := func(id, created string) map[string]any {
		return map[string]any{"id": id, "name": "Ubuntu 24.04 LTS 64-bit", "status": "active", "created_at": created}
	}

	f := newFakeOpenStack(t)
	f.handle("GET /image/v2/images", respond(200, map[string]any{"images": []any{
		image("old", "2026-01-01T00:00:00Z"),
		image("new", "2026-09-01T00:00:00Z"),
		image("mid", "2026-05-01T00:00:00Z"),
	}}))

	id, err := f.client(t).LatestImageByName(context.Background(), "Ubuntu 24.04 LTS 64-bit", "ru-9a")
	if err != nil || id != "new" {
		t.Fatalf("LatestImageByName() = %q, %v", id, err)
	}

	q := f.requestsTo("GET", "/image/v2/images")[0].Query
	if q.Get("name") != "Ubuntu 24.04 LTS 64-bit" || q.Get("status") != "active" {
		t.Fatalf("images query = %v", q)
	}
}

func TestLatestImageByNameNotFound(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /image/v2/images", respond(200, map[string]any{"images": []any{}}))

	if _, err := f.client(t).LatestImageByName(context.Background(), "Nope", "ru-9a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestImageByName() = %v, want ErrNotFound", err)
	}
}

// Собранный из диска образ лежит только в сторе своей зоны: в другой зоне
// берётся свежий из тех, что там есть.
func TestLatestImageByNameFiltersByStore(t *testing.T) {
	image := func(id, created, stores string) map[string]any {
		return map[string]any{"id": id, "name": "worker", "status": "active", "created_at": created, "stores": stores}
	}

	f := newFakeOpenStack(t)
	f.handle("GET /image/v2/images", respond(200, map[string]any{"images": []any{
		image("a-new", "2026-10-08T00:00:00Z", "ru-9a"),
		image("b-old", "2026-09-01T00:00:00Z", "ru-9b"),
		image("both", "2026-08-01T00:00:00Z", "ru-9a,ru-9b"),
	}}))

	for zone, want := range map[string]string{"ru-9a": "a-new", "ru-9b": "b-old"} {
		if id, err := f.client(t).LatestImageByName(context.Background(), "worker", zone); err != nil || id != want {
			t.Fatalf("LatestImageByName(%s) = %q, %v, want %s", zone, id, err, want)
		}
	}
	if _, err := f.client(t).LatestImageByName(context.Background(), "worker", "ru-9c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestImageByName(ru-9c) = %v, want ErrNotFound", err)
	}
}

func flavorList() http.HandlerFunc {
	return respond(200, map[string]any{"flavors": []any{
		map[string]any{"id": "1011", "name": "SL1.2-4096", "vcpus": 2, "ram": 4096, "disk": 0, "os-flavor-access:is_public": true},
		// публичный с такими же размерами — не наш, переиспользовать нельзя
		map[string]any{"id": "2022", "name": "HF1.4-8192", "vcpus": 4, "ram": 8192, "disk": 0, "os-flavor-access:is_public": true},
		map[string]any{"id": "own", "name": "fleeting-2-512-aaaa0000", "vcpus": 2, "ram": 512, "disk": 0, "os-flavor-access:is_public": false},
	}})
}

func TestFlavorID(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /compute/v2.1/flavors/detail", flavorList())
	c := f.client(t)

	for _, nameOrID := range []string{"SL1.2-4096", "1011"} {
		if id, err := c.FlavorID(context.Background(), nameOrID); err != nil || id != "1011" {
			t.Fatalf("FlavorID(%q) = %q, %v", nameOrID, id, err)
		}
	}
	if _, err := c.FlavorID(context.Background(), "SL1.99"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FlavorID(unknown) = %v, want ErrNotFound", err)
	}
}

func TestCustomFlavor(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("GET /compute/v2.1/flavors/detail", flavorList())
	f.handle("POST /compute/v2.1/flavors", respond(200, map[string]any{"flavor": map[string]any{"id": "new-flavor"}}))
	c := f.client(t)

	if id, err := c.CustomFlavor(context.Background(), 2, 512); err != nil || id != "own" {
		t.Fatalf("CustomFlavor(2, 512) = %q, %v, want the existing own flavor", id, err)
	}
	if len(f.requestsTo("POST", "/compute/v2.1/flavors")) != 0 {
		t.Fatal("flavor created although an own one exists")
	}

	id, err := c.CustomFlavor(context.Background(), 4, 8192)
	if err != nil || id != "new-flavor" {
		t.Fatalf("CustomFlavor(4, 8192) = %q, %v, want a new flavor", id, err)
	}

	body := f.requestsTo("POST", "/compute/v2.1/flavors")[0].Body["flavor"].(map[string]any)
	name, _ := body["name"].(string)
	if !strings.HasPrefix(name, "fleeting-4-8192-") || body["vcpus"] != float64(4) || body["ram"] != float64(8192) ||
		body["disk"] != float64(0) || body["os-flavor-access:is_public"] != false {
		t.Fatalf("flavor request = %v", body)
	}
}

func TestExternalNetworkID(t *testing.T) {
	network := func(id string) map[string]any { return map[string]any{"id": id, "name": "external-network"} }

	tests := []struct {
		name     string
		networks []any
		want     string
		wantErr  string
	}{
		{name: "one", networks: []any{network("ext-1")}, want: "ext-1"},
		{name: "none", networks: []any{}, wantErr: "no external network"},
		{name: "several", networks: []any{network("ext-1"), network("ext-2")}, wantErr: "set floating_network_id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeOpenStack(t)
			f.handle("GET /network/v2.0/networks", respond(200, map[string]any{"networks": tt.networks}))

			id, err := f.client(t).ExternalNetworkID(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ExternalNetworkID() = %q, %v, want %q", id, err, tt.wantErr)
				}
				return
			}
			if err != nil || id != tt.want {
				t.Fatalf("ExternalNetworkID() = %q, %v", id, err)
			}
			if q := f.requestsTo("GET", "/network/v2.0/networks")[0].Query; q.Get("router:external") != "true" {
				t.Fatalf("networks query = %v", q)
			}
		})
	}
}

func TestKeypairs(t *testing.T) {
	f := newFakeOpenStack(t)
	f.handle("POST /compute/v2.1/os-keypairs", respond(200, map[string]any{"keypair": map[string]any{"name": "fleeting-runner-1"}}))
	f.handle("DELETE /compute/v2.1/os-keypairs/fleeting-runner-1", noContent)
	f.handle("DELETE /compute/v2.1/os-keypairs/gone", respond(404, map[string]any{"itemNotFound": map[string]any{"message": "gone"}}))
	c := f.client(t)

	if err := c.CreateKeypair(context.Background(), "fleeting-runner-1", "ssh-ed25519 AAAA"); err != nil {
		t.Fatalf("CreateKeypair() = %v", err)
	}
	body := f.requestsTo("POST", "/compute/v2.1/os-keypairs")[0].Body["keypair"]
	if want := map[string]any{"name": "fleeting-runner-1", "public_key": "ssh-ed25519 AAAA"}; !reflect.DeepEqual(body, want) {
		t.Fatalf("keypair request = %v", body)
	}

	if err := c.DeleteKeypair(context.Background(), "fleeting-runner-1"); err != nil {
		t.Fatalf("DeleteKeypair() = %v", err)
	}
	// уже удалённая — не ошибка
	if err := c.DeleteKeypair(context.Background(), "gone"); err != nil {
		t.Fatalf("DeleteKeypair(gone) = %v", err)
	}
}

func TestMapError(t *testing.T) {
	codeErr := func(code int, body string) error {
		return gophercloud.ErrUnexpectedResponseCode{Actual: code, Body: []byte(body)}
	}

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"not found", codeErr(404, `{"itemNotFound": {}}`), ErrNotFound},
		{"nova quota", codeErr(403, `{"forbidden": {"message": "Quota exceeded for cores"}}`), ErrResourceExhausted},
		{"neutron quota", codeErr(409, `{"NeutronError": {"type": "OverQuota"}}`), ErrResourceExhausted},
		{"volume quota", codeErr(413, `{"overLimit": {"message": "VolumeLimitExceeded"}}`), ErrResourceExhausted},
		{"forbidden", codeErr(403, `{"forbidden": {"message": "Policy doesn't allow"}}`), nil},
		{"bad request", codeErr(400, `{"badRequest": {}}`), nil},
		{"internal error", codeErr(500, `{"computeFault": {}}`), ErrUnavailable},
		{"service unavailable", codeErr(503, ``), ErrUnavailable},
		{"transport", errors.New("connection refused"), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapError(tt.err)
			// ErrUnexpectedResponseCode несравнима, errors.Is её не находит
			var codeErr gophercloud.ErrUnexpectedResponseCode
			if !errors.Is(got, tt.err) && !errors.As(got, &codeErr) {
				t.Fatalf("mapError() lost the original error: %v", got)
			}
			for _, sentinel := range []error{ErrNotFound, ErrResourceExhausted, ErrUnavailable} {
				if errors.Is(got, sentinel) != errors.Is(sentinel, tt.want) {
					t.Fatalf("mapError(%v) = %v, want sentinel %v", tt.err, got, tt.want)
				}
			}
		})
	}
}

func TestRandomHex(t *testing.T) {
	if a, b := randomHex(), randomHex(); len(a) != 8 || a == b {
		t.Fatalf("randomHex() = %q, %q", a, b)
	}
}
