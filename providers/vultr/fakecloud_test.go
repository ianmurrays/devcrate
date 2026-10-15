package vultr

// A fake Vultr API (v2), served in-process to the Provider's real curl
// through VULTR_API_URL. It keeps the account in memory and logs every call.
// Like the real one, a new instance is pending (main_ip 0.0.0.0) for a while,
// and a volume stays attached for a while after its instance is destroyed:
// both last a few requests ("ticks") here.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	pendingTicks = 2  // requests a new instance stays pending for
	detachTicks  = 15 // requests a volume stays attached after its instance is destroyed
)

type fakeVultr struct {
	*httptest.Server
	mu        sync.Mutex
	key       string
	next      int
	instances []*instance
	blocks    []*block
	sshKeys   []*sshKey
	regions   []region
	images    []osImage
	calls     []string
	// userData is what each instance create sent, decoded, by label.
	userData map[string]string
	// inactive makes every create fail as an account flagged for inactivity does.
	inactive bool
}

type instance struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Hostname    string   `json:"hostname"`
	Region      string   `json:"region"`
	Plan        string   `json:"plan"`
	OSID        int      `json:"os_id"`
	SnapshotID  string   `json:"snapshot_id"`
	MainIP      string   `json:"main_ip"`
	Status      string   `json:"status"`
	PowerStatus string   `json:"power_status"`
	Tags        []string `json:"tags"`
	SSHKeys     []string `json:"-"`
	pending     int
}

type block struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Region     string `json:"region"`
	SizeGB     int    `json:"size_gb"`
	BlockType  string `json:"block_type"`
	Status     string `json:"status"`
	AttachedTo string `json:"attached_to_instance"`
	detaching  int
}

type sshKey struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	SSHKey string `json:"ssh_key"`
}

type region struct {
	ID      string   `json:"id"`
	Options []string `json:"options"`
}

type osImage struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func newFakeVultr(t *testing.T, key string) *fakeVultr {
	v := &fakeVultr{key: key, next: 1000, userData: map[string]string{},
		regions: []region{
			{"ewr", []string{"ddos_protection", "block_storage_high_perf", "block_storage_storage_opt"}},
			{"ams", []string{"block_storage_high_perf"}},
			{"sao", []string{"ddos_protection", "block_storage_storage_opt"}},
		},
		images: []osImage{
			{1743, "Ubuntu 22.04 LTS x64"},
			{2284, "Ubuntu 24.04 LTS x64"},
			{2465, "Ubuntu 24.10 x64"},
			{2285, "Ubuntu 26.04 LTS arm64"},
			{2136, "Debian 12 x64 (bookworm)"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/regions", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"regions": v.regions})
	})
	mux.HandleFunc("GET /v2/os", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"os": v.images})
	})
	mux.HandleFunc("GET /v2/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"ssh_keys": orEmpty(v.sshKeys)})
	})
	mux.HandleFunc("POST /v2/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name   string
			SSHKey string `json:"ssh_key"`
		}
		if !decode(w, r, &body) {
			return
		}
		k := &sshKey{ID: v.id(), Name: body.Name, SSHKey: body.SSHKey}
		v.sshKeys = append(v.sshKeys, k)
		reply(w, 201, map[string]any{"ssh_key": k})
	})
	mux.HandleFunc("GET /v2/instances", func(w http.ResponseWriter, r *http.Request) {
		out := []*instance{}
		for _, i := range v.instances {
			if t := r.URL.Query().Get("tag"); t == "" || slices.Contains(i.Tags, t) {
				out = append(out, i)
			}
		}
		reply(w, 200, map[string]any{"instances": out})
	})
	mux.HandleFunc("POST /v2/instances", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Region, Plan, Label, Hostname string
			OSID                          int      `json:"os_id"`
			SnapshotID                    string   `json:"snapshot_id"`
			SSHKeyID                      []string `json:"sshkey_id"`
			UserData                      string   `json:"user_data"`
			Tags                          []string
		}
		if !decode(w, r, &body) || !v.creatable(w) {
			return
		}
		switch {
		case v.region(body.Region) == nil:
			fail(w, 400, "Invalid region.")
		case body.Plan == "":
			fail(w, 400, "Invalid plan.")
		case (body.OSID == 0) == (body.SnapshotID == ""):
			fail(w, 400, "Specify exactly one of os_id or snapshot_id.")
		default:
			ud, err := base64.StdEncoding.DecodeString(body.UserData)
			if err != nil {
				fail(w, 400, "user_data must be base64.")
				return
			}
			i := &instance{ID: v.id(), Label: body.Label, Hostname: body.Hostname, Region: body.Region, Plan: body.Plan,
				OSID: body.OSID, SnapshotID: body.SnapshotID, MainIP: "0.0.0.0", Status: "pending", PowerStatus: "running",
				Tags: body.Tags, SSHKeys: body.SSHKeyID, pending: pendingTicks}
			v.instances = append(v.instances, i)
			v.userData[i.Label] = string(ud)
			reply(w, 202, map[string]any{"instance": i})
		}
	})
	mux.HandleFunc("DELETE /v2/instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !v.deleteInstance(r.PathValue("id")) {
			fail(w, 404, "Invalid instance-id.")
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v2/instances/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		i := v.instance(r.PathValue("id"))
		if i == nil {
			fail(w, 404, "Invalid instance-id.")
			return
		}
		i.PowerStatus = "running"
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v2/blocks", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"blocks": orEmpty(v.blocks)})
	})
	mux.HandleFunc("POST /v2/blocks", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Region, Label string
			SizeGB        int    `json:"size_gb"`
			BlockType     string `json:"block_type"`
		}
		if !decode(w, r, &body) || !v.creatable(w) {
			return
		}
		reg := v.region(body.Region)
		switch {
		case reg == nil:
			fail(w, 400, "Invalid region.")
		case body.BlockType != "high_perf" && body.BlockType != "storage_opt":
			fail(w, 400, "Invalid block_type.")
		case !slices.Contains(reg.Options, "block_storage_"+body.BlockType):
			fail(w, 400, "Block storage type is not available in this region.")
		case body.SizeGB < 10 || body.SizeGB > 10000:
			fail(w, 400, "Invalid size_gb.")
		default:
			b := &block{ID: v.id(), Label: body.Label, Region: body.Region, SizeGB: body.SizeGB, BlockType: body.BlockType, Status: "active"}
			v.blocks = append(v.blocks, b)
			reply(w, 202, map[string]any{"block": b})
		}
	})
	mux.HandleFunc("POST /v2/blocks/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			InstanceID string `json:"instance_id"`
			Live       *bool
		}
		if !decode(w, r, &body) {
			return
		}
		b, i := v.block(r.PathValue("id")), v.instance(body.InstanceID)
		switch {
		case b == nil || i == nil:
			fail(w, 404, "Not found.")
		case body.Live == nil || !*body.Live:
			fail(w, 400, "the fake only attaches live")
		case i.Status != "active":
			fail(w, 400, "Unable to attach block storage: server is not ready.")
		case b.AttachedTo != "":
			fail(w, 400, "Block storage is already attached to a server.")
		case b.Region != i.Region:
			fail(w, 400, "Block storage and server are in different regions.")
		default:
			b.AttachedTo = i.ID
			w.WriteHeader(204)
		}
	})
	mux.HandleFunc("DELETE /v2/blocks/{id}", func(w http.ResponseWriter, r *http.Request) {
		b := v.block(r.PathValue("id"))
		switch {
		case b == nil:
			fail(w, 404, "Invalid block-id.")
		case b.AttachedTo != "":
			fail(w, 400, "Block storage is attached to a server.")
		default:
			v.blocks = slices.DeleteFunc(v.blocks, func(x *block) bool { return x == b })
			w.WriteHeader(204)
		}
	})

	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.calls = append(v.calls, r.Method+" "+r.URL.RequestURI())
		defer v.tick()
		if r.Header.Get("Authorization") != "Bearer "+v.key {
			fail(w, 401, "Invalid API token.")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(v.Close)
	return v
}

// tick advances the fake clock after each request.
func (v *fakeVultr) tick() {
	for _, i := range v.instances {
		if i.pending > 0 {
			if i.pending--; i.pending == 0 {
				i.Status, i.MainIP = "active", "203.0.113."+strconv.Itoa(len(v.calls)%250)
			}
		}
	}
	for _, b := range v.blocks {
		if b.detaching > 0 {
			if b.detaching--; b.detaching == 0 {
				b.AttachedTo = ""
			}
		}
	}
}

// deleteInstance destroys an instance; its volume detaches after a while
// and survives.
func (v *fakeVultr) deleteInstance(id string) bool {
	i := v.instance(id)
	if i == nil {
		return false
	}
	v.instances = slices.DeleteFunc(v.instances, func(x *instance) bool { return x == i })
	for _, b := range v.blocks {
		if b.AttachedTo == id {
			b.detaching = detachTicks
		}
	}
	return true
}

// creatable fails creates on an account flagged for inactivity.
func (v *fakeVultr) creatable(w http.ResponseWriter) bool {
	if v.inactive {
		fail(w, 400, "Due to account inactivity, please verify your payment details")
	}
	return !v.inactive
}

func (v *fakeVultr) id() string {
	v.next++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", v.next)
}

func (v *fakeVultr) instance(id string) *instance {
	for _, i := range v.instances {
		if i.ID == id {
			return i
		}
	}
	return nil
}

func (v *fakeVultr) block(id string) *block {
	for _, b := range v.blocks {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (v *fakeVultr) region(id string) *region {
	for i := range v.regions {
		if v.regions[i].ID == id {
			return &v.regions[i]
		}
	}
	return nil
}

// decode reads a JSON body; like the real API, it ignores the body unless
// the request says it is JSON.
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		fail(w, 400, "the fake needs Content-Type: application/json, or Vultr ignores the body")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		fail(w, 400, "Bad request: "+err.Error())
		return false
	}
	return true
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]any{"error": msg, "status": code})
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Lock-free reads for tests, between Provider runs.
func (v *fakeVultr) callsWith(substr string) []string {
	var out []string
	for _, c := range v.calls {
		if strings.Contains(c, substr) {
			out = append(out, c)
		}
	}
	return out
}
