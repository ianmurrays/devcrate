package vultr

// A fake Vultr API (v2), served in-process to the Provider's real curl
// through VULTR_API_URL. It keeps the account in memory and logs every call.
// Like the real one, a new instance is pending (main_ip 0.0.0.0) for a while,
// and a volume stays attached for a while after its instance is destroyed:
// both last a few requests ("ticks") here.
//
// It also keeps IAM: service users, their API keys and IP allowlists, and
// policies. A service user's key is checked like the real one: only from an
// allowlisted IP (the fake takes the caller's IP from from), and only for an
// instance delete its policies grant.

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
	users    []*user
	policies []*policy
	// from is the IP every request comes from: the laptop's unless a test
	// sets an instance's, as when the instance runs the self-stop script.
	from string
}

type user struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	ServiceUser bool   `json:"service_user"`
	APIEnabled  bool   `json:"api_enabled"`
	keys        []*userKey
	allowlist   []allowed
	policies    []string
}

type userKey struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
	Name   string `json:"name"`
	Expire bool   `json:"expire"`
}

type allowed struct {
	Subnet     string `json:"subnet"`
	SubnetSize int    `json:"subnet_size"`
}

type policy struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Document policyDocument `json:"policy_document"`
	System   bool           `json:"is_system_policy"`
}

type policyDocument struct {
	Version   string
	Statement []struct {
		Action   []string
		Effect   string
		Resource []string
	}
}

const laptopIP = "198.51.100.7"

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
	v := &fakeVultr{key: key, next: 1000, userData: map[string]string{}, from: laptopIP,
		policies: []*policy{{ID: "6f11fb82-0000-4000-8000-000000000001", Name: "Manage DNS", System: true}},
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

	mux.HandleFunc("GET /v2/users", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"users": orEmpty(v.users)})
	})
	mux.HandleFunc("POST /v2/users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name, Email, Password string
			ServiceUser           bool `json:"service_user"`
			APIEnabled            bool `json:"api_enabled"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Name == "" || body.Email == "" || body.Password == "" {
			fail(w, 400, "Bad request.")
			return
		}
		// Like the real API, a new user comes with a key named Default.
		u := &user{ID: v.id(), Name: body.Name, Email: body.Email, ServiceUser: body.ServiceUser, APIEnabled: body.APIEnabled}
		k := v.newKey(u, "Default")
		reply(w, 201, map[string]any{"user": map[string]any{"id": u.ID, "name": u.Name, "email": u.Email,
			"service_user": u.ServiceUser, "api_enabled": u.APIEnabled, "api_key": k.APIKey}})
		v.users = append(v.users, u)
	})
	mux.HandleFunc("DELETE /v2/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		u := v.user(r.PathValue("id"))
		if u == nil {
			fail(w, 404, "User not found.")
			return
		}
		v.users = slices.DeleteFunc(v.users, func(x *user) bool { return x == u })
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v2/users/{id}/policies", func(w http.ResponseWriter, r *http.Request) {
		u := v.user(r.PathValue("id"))
		if u == nil {
			fail(w, 404, "User not found.")
			return
		}
		direct := []*policy{}
		for _, id := range u.policies {
			direct = append(direct, v.policy(id))
		}
		reply(w, 200, map[string]any{"user_id": u.ID, "policies": map[string]any{"direct": direct, "all": direct}})
	})
	mux.HandleFunc("GET /v2/users/{id}/apikeys", func(w http.ResponseWriter, r *http.Request) {
		u := v.user(r.PathValue("id"))
		if u == nil {
			fail(w, 404, "User not found.")
			return
		}
		masked := []userKey{}
		for _, k := range u.keys {
			m := *k
			m.APIKey = "*******" + k.APIKey[len(k.APIKey)-5:]
			masked = append(masked, m)
		}
		reply(w, 200, map[string]any{"api_keys": masked})
	})
	mux.HandleFunc("POST /v2/users/{id}/apikeys", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name   string
			Expire *bool
		}
		u := v.user(r.PathValue("id"))
		if !decode(w, r, &body) {
			return
		}
		switch {
		case u == nil:
			fail(w, 404, "User not found.")
		case body.Expire == nil || *body.Expire:
			fail(w, 400, "the fake only mints keys that never expire")
		default:
			reply(w, 202, map[string]any{"api_key": v.newKey(u, body.Name)})
		}
	})
	mux.HandleFunc("DELETE /v2/users/{id}/apikeys/{key}", func(w http.ResponseWriter, r *http.Request) {
		u := v.user(r.PathValue("id"))
		if u == nil || !slices.ContainsFunc(u.keys, func(k *userKey) bool { return k.ID == r.PathValue("key") }) {
			fail(w, 404, "API key not found")
			return
		}
		u.keys = slices.DeleteFunc(u.keys, func(k *userKey) bool { return k.ID == r.PathValue("key") })
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v2/users/{id}/ip-whitelist", func(w http.ResponseWriter, r *http.Request) {
		u := v.user(r.PathValue("id"))
		if u == nil {
			fail(w, 404, "User not found.")
			return
		}
		reply(w, 200, map[string]any{"ip_whitelist": orEmpty(u.allowlist)})
	})
	allowlist := func(w http.ResponseWriter, r *http.Request) {
		var body allowed
		u := v.user(r.PathValue("id"))
		if !decode(w, r, &body) {
			return
		}
		if u == nil {
			fail(w, 404, "User not found.")
			return
		}
		u.allowlist = slices.DeleteFunc(u.allowlist, func(a allowed) bool { return a == body })
		if r.Method == "POST" {
			u.allowlist = append(u.allowlist, body)
		}
		w.WriteHeader(204)
	}
	mux.HandleFunc("POST /v2/users/{id}/ip-whitelist", allowlist)
	mux.HandleFunc("DELETE /v2/users/{id}/ip-whitelist", allowlist)
	mux.HandleFunc("GET /v2/policies", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"policies": v.policies})
	})
	// POST and PUT reply with the bare policy; GET /policies/{id} would
	// wrap it in "policy".
	mux.HandleFunc("POST /v2/policies", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name     string
			Document *policyDocument `json:"policy_document"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Name == "" || body.Document == nil {
			fail(w, 422, "name and policy_document are required")
			return
		}
		p := &policy{ID: v.id(), Name: body.Name, Document: *body.Document}
		v.policies = append(v.policies, p)
		reply(w, 201, p)
	})
	mux.HandleFunc("PUT /v2/policies/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Document *policyDocument `json:"policy_document"`
		}
		p := v.policy(r.PathValue("id"))
		if !decode(w, r, &body) {
			return
		}
		if p == nil || p.System {
			fail(w, 404, "Policy Not Found")
			return
		}
		if body.Document != nil {
			p.Document = *body.Document
		}
		reply(w, 202, p)
	})
	mux.HandleFunc("DELETE /v2/policies/{id}", func(w http.ResponseWriter, r *http.Request) {
		p := v.policy(r.PathValue("id"))
		if p == nil || p.System {
			fail(w, 404, "Policy Not Found")
			return
		}
		v.policies = slices.DeleteFunc(v.policies, func(x *policy) bool { return x == p })
		for _, u := range v.users {
			u.policies = slices.DeleteFunc(u.policies, func(id string) bool { return id == p.ID })
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v2/policies/{id}/users/{user}", func(w http.ResponseWriter, r *http.Request) {
		p, u := v.policy(r.PathValue("id")), v.user(r.PathValue("user"))
		switch {
		case p == nil || u == nil:
			fail(w, 404, "Not found.")
		case slices.Contains(u.policies, p.ID):
			fail(w, 409, "Policy User Relationship Already Exists")
		default:
			u.policies = append(u.policies, p.ID)
			reply(w, 201, map[string]any{"user_id": u.ID, "policy_id": p.ID})
		}
	})

	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.calls = append(v.calls, r.Method+" "+r.URL.RequestURI())
		defer v.tick()
		// The metadata service answers the instance that asks, with no key.
		if r.Method == "GET" && r.URL.Path == "/v1/instance-v2-id" {
			for _, i := range v.instances {
				if i.MainIP == v.from {
					fmt.Fprint(w, i.ID)
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		if bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); bearer != v.key {
			v.asServiceUser(w, r, bearer, mux)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(v.Close)
	return v
}

// asServiceUser serves a request made with a service user's key: only from
// an IP on the user's allowlist, and only an instance delete that one of the
// user's policies allows on that instance.
func (v *fakeVultr) asServiceUser(w http.ResponseWriter, r *http.Request, bearer string, mux *http.ServeMux) {
	var u *user
	for _, x := range v.users {
		if slices.ContainsFunc(x.keys, func(k *userKey) bool { return k.APIKey == bearer }) {
			u = x
		}
	}
	switch {
	case u == nil:
		fail(w, 401, "Invalid API token.")
		return
	case !slices.Contains(u.allowlist, allowed{v.from, 32}):
		fail(w, 401, "Unauthorized IP address: "+v.from)
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/v2/instances/")
	if r.Method == "DELETE" && ok && !strings.Contains(id, "/") {
		for _, pid := range u.policies {
			for _, st := range v.policy(pid).Document.Statement {
				if st.Effect == "Allow" && slices.Contains(st.Action, "compute.instance.Delete") && slices.Contains(st.Resource, "instance:"+id) {
					mux.ServeHTTP(w, r)
					return
				}
			}
		}
	}
	fail(w, 403, "Permission denied. You do not have the required IAM permissions for this action.")
}

func (v *fakeVultr) newKey(u *user, name string) *userKey {
	k := &userKey{ID: v.id(), APIKey: fmt.Sprintf("SVCKEY%030d", v.next), Name: name}
	u.keys = append(u.keys, k)
	return k
}

func (v *fakeVultr) user(id string) *user {
	for _, u := range v.users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (v *fakeVultr) policy(id string) *policy {
	for _, p := range v.policies {
		if p.ID == id {
			return p
		}
	}
	return nil
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
