package digitalocean

// A fake DigitalOcean: the test binary, linked as `doctl` and `curl` on the
// Provider's PATH, answers the doctl commands and API calls the Provider
// makes from a JSON state file ($FAKE_CLOUD), and logs every call to it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type cloud struct {
	NextID   int        `json:"next_id"`
	Droplets []*droplet `json:"droplets"`
	Volumes  []*volume  `json:"volumes"`
	SSHKeys  []*sshKey  `json:"ssh_keys"`
	Tags     []string   `json:"tags"`
	// Snapshots are complete snapshots, as the API lists them.
	Snapshots []*snapshot `json:"snapshots"`
	Calls     [][]string  `json:"calls"`
	// UserData is what each droplet create sent, by droplet name.
	UserData map[string]string `json:"user_data"`
}

type droplet struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Region   string   `json:"region"` // doctl prints an object; the Provider never reads it
	Size     string   `json:"size_slug"`
	Image    string   `json:"image"`
	SSHKeys  []string `json:"ssh_keys"`
	Volumes  []string `json:"volume_ids"`
	Tags     []string `json:"tags"`
	Networks struct {
		V4 []struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

type volume struct {
	ID       string                `json:"id"`
	Name     string                `json:"name"`
	Region   struct{ Slug string } `json:"region"`
	SizeGiB  int                   `json:"size_gigabytes"`
	Droplets []int                 `json:"droplet_ids"`
	Tags     []string              `json:"tags"`
}

type snapshot struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Created    string   `json:"created_at"`
	Regions    []string `json:"regions"`
	ResourceID string   `json:"resource_id"`
}

type sshKey struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

func loadCloud(path string) *cloud {
	c := &cloud{NextID: 1000, UserData: map[string]string{}}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, c); err != nil {
			panic(err)
		}
	}
	return c
}

func (c *cloud) save(path string) {
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}

func (c *cloud) id() int { c.NextID++; return c.NextID }

func (c *cloud) tag(t string) {
	if t != "" && !slices.Contains(c.Tags, t) {
		c.Tags = append(c.Tags, t)
	}
}

// flags splits doctl-style args into positionals and --flag values.
func flags(args []string) (pos []string, f map[string]string) {
	f = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") && a != "-o" {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(name, "="); ok {
			f[k] = v
			continue
		}
		switch name {
		case "force", "wait":
			f[name] = "true"
		default:
			i++
			f[name] = args[i]
		}
	}
	return pos, f
}

func printJSON(v any) { json.NewEncoder(os.Stdout).Encode(v) }

func doctlFail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	return 1
}

// fakeDoctl implements the doctl subcommands the Provider uses.
func fakeDoctl(args []string) int {
	path := os.Getenv("FAKE_CLOUD")
	c := loadCloud(path)
	c.Calls = append(c.Calls, append([]string{"doctl"}, args...))
	defer c.save(path)
	if os.Getenv("FAKE_DOCTL_FAIL") != "" && strings.Contains(strings.Join(args, " "), os.Getenv("FAKE_DOCTL_FAIL")) {
		return doctlFail("failing as asked")
	}

	pos, f := flags(args)
	cmd := strings.Join(pos[:min(3, len(pos))], " ")
	switch {
	case cmd == "compute ssh-key list":
		printJSON(c.SSHKeys)
	case strings.HasPrefix(cmd, "compute ssh-key create"):
		k := &sshKey{ID: c.id(), Name: pos[3], PublicKey: f["public-key"]}
		c.SSHKeys = append(c.SSHKeys, k)
		printJSON([]*sshKey{k})
	case cmd == "compute volume list":
		printJSON(c.Volumes)
	case strings.HasPrefix(cmd, "compute volume create"):
		size, err := strconv.Atoi(strings.TrimSuffix(f["size"], "GiB"))
		if err != nil {
			return doctlFail("invalid size %q", f["size"])
		}
		v := &volume{ID: fmt.Sprintf("vol-%d", c.id()), Name: pos[3], SizeGiB: size, Droplets: []int{}}
		v.Region.Slug = f["region"]
		if f["tag"] != "" {
			v.Tags = []string{f["tag"]}
			c.tag(f["tag"])
		}
		c.Volumes = append(c.Volumes, v)
		printJSON([]*volume{v})
	case strings.HasPrefix(cmd, "compute volume delete"):
		for i, v := range c.Volumes {
			if v.ID == pos[3] {
				if len(v.Droplets) > 0 {
					return doctlFail("volume is attached")
				}
				c.Volumes = slices.Delete(c.Volumes, i, i+1)
				return 0
			}
		}
		return doctlFail("volume %s not found", pos[3])
	case cmd == "compute droplet list":
		out := []*droplet{}
		for _, d := range c.Droplets {
			if f["tag-name"] == "" || slices.Contains(d.Tags, f["tag-name"]) {
				out = append(out, d)
			}
		}
		printJSON(out)
	case strings.HasPrefix(cmd, "compute droplet create"):
		if f["image"] == "" {
			return doctlFail("(droplet.create.image) command is missing required arguments")
		}
		d := &droplet{ID: c.id(), Name: pos[3], Status: "active", Region: f["region"], Size: f["size"], Image: f["image"]}
		d.Networks.V4 = append(d.Networks.V4, struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		}{"10.1.0." + strconv.Itoa(d.ID%250), "private"}, struct {
			IP   string `json:"ip_address"`
			Type string `json:"type"`
		}{"203.0.113." + strconv.Itoa(d.ID%250), "public"})
		if f["ssh-keys"] != "" {
			d.SSHKeys = strings.Split(f["ssh-keys"], ",")
		}
		if f["tag-name"] != "" {
			d.Tags = []string{f["tag-name"]}
			c.tag(f["tag-name"])
		}
		for _, id := range strings.Split(f["volumes"], ",") {
			for _, v := range c.Volumes {
				if v.ID == id {
					if v.Region.Slug != f["region"] {
						return doctlFail("volume %s is in %s, not %s", id, v.Region.Slug, f["region"])
					}
					v.Droplets = append(v.Droplets, d.ID)
					d.Volumes = append(d.Volumes, id)
				}
			}
		}
		if ud, err := os.ReadFile(f["user-data-file"]); err == nil {
			c.UserData[d.Name] = string(ud)
		}
		c.Droplets = append(c.Droplets, d)
		printJSON([]*droplet{d})
	case strings.HasPrefix(cmd, "compute droplet delete"):
		id, _ := strconv.Atoi(pos[3])
		if !c.deleteDroplet(id) {
			return doctlFail("droplet %d not found", id)
		}
	case strings.HasPrefix(cmd, "compute droplet-action power-on"):
		id, _ := strconv.Atoi(pos[3])
		for _, d := range c.Droplets {
			if d.ID == id {
				d.Status = "active"
			}
		}
		printJSON([]map[string]any{{"status": "completed"}})
	case strings.HasPrefix(cmd, "compute droplet-action power-off"),
		strings.HasPrefix(cmd, "compute droplet-action snapshot"):
		id, _ := strconv.Atoi(pos[3])
		d := c.droplet(id)
		if d == nil {
			return doctlFail("droplet %d not found", id)
		}
		// Without --wait the action is still in progress: the droplet
		// stays on and no snapshot is complete yet.
		if f["wait"] == "" {
			printJSON([]map[string]any{{"status": "in-progress"}})
			return 0
		}
		if pos[2] == "power-off" {
			d.Status = "off"
		} else {
			if d.Status != "off" {
				return doctlFail("fake: snapshotting a droplet that is %s, not off", d.Status)
			}
			// created_at grows with each new ID, so it orders snapshots.
			n := c.id()
			c.Snapshots = append(c.Snapshots, &snapshot{ID: strconv.Itoa(n), Name: f["snapshot-name"],
				Created: time.Unix(1700000000+int64(n), 0).UTC().Format(time.RFC3339), Regions: []string{d.Region}, ResourceID: strconv.Itoa(id)})
		}
		printJSON([]map[string]any{{"status": "completed"}})
	case cmd == "compute snapshot list":
		if f["resource"] != "droplet" {
			return doctlFail("fake: snapshot list without --resource droplet")
		}
		printJSON(c.Snapshots)
	case strings.HasPrefix(cmd, "compute snapshot delete"):
		i := slices.IndexFunc(c.Snapshots, func(s *snapshot) bool { return s.ID == pos[3] })
		if i < 0 {
			return doctlFail("snapshot %s not found", pos[3])
		}
		c.Snapshots = slices.Delete(c.Snapshots, i, i+1)
	case strings.HasPrefix(cmd, "compute tag get"):
		if !slices.Contains(c.Tags, pos[3]) {
			return doctlFail("tag %s not found", pos[3])
		}
		printJSON([]map[string]string{{"name": pos[3]}})
	case strings.HasPrefix(cmd, "compute tag delete"):
		i := slices.Index(c.Tags, pos[3])
		if i < 0 {
			return doctlFail("tag %s not found", pos[3])
		}
		c.Tags = slices.Delete(c.Tags, i, i+1)
	default:
		return doctlFail("fake doctl: unknown command %q", args)
	}
	return 0
}

func (c *cloud) droplet(id int) *droplet {
	for _, d := range c.Droplets {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// deleteDroplet destroys a droplet; its volumes detach and survive.
func (c *cloud) deleteDroplet(id int) bool {
	for i, d := range c.Droplets {
		if d.ID == id {
			for _, v := range c.Volumes {
				v.Droplets = slices.DeleteFunc(v.Droplets, func(x int) bool { return x == id })
			}
			c.Droplets = slices.Delete(c.Droplets, i, i+1)
			return true
		}
	}
	return false
}

// fakeCurl serves the droplet metadata service ($FAKE_DROPLET_ID is the
// droplet "this VM" is) and DELETE /v2/droplets/<id> of the API.
func fakeCurl(args []string) int {
	path := os.Getenv("FAKE_CLOUD")
	c := loadCloud(path)
	c.Calls = append(c.Calls, append([]string{"curl"}, args...))
	defer c.save(path)

	method, url, auth := "GET", "", ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-X":
			i++
			method = args[i]
		case a == "-H":
			i++
			auth, _ = strings.CutPrefix(args[i], "Authorization: Bearer ")
		case strings.HasPrefix(a, "http"):
			url = a
		}
	}
	switch {
	case method == "GET" && url == "http://169.254.169.254/metadata/v1/id":
		fmt.Print(os.Getenv("FAKE_DROPLET_ID"))
	case method == "DELETE" && strings.HasPrefix(url, "https://api.digitalocean.com/v2/droplets/"):
		if auth == "" || auth != os.Getenv("FAKE_API_TOKEN") {
			fmt.Fprintln(os.Stderr, "curl: (22) The requested URL returned error: 401")
			return 22
		}
		id, err := strconv.Atoi(filepath.Base(url))
		if err != nil || !c.deleteDroplet(id) {
			fmt.Fprintln(os.Stderr, "curl: (22) The requested URL returned error: 404")
			return 22
		}
	default:
		fmt.Fprintf(os.Stderr, "fake curl: unexpected %s %s\n", method, url)
		return 1
	}
	return 0
}
