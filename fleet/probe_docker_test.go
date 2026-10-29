//go:build fleetdocker

package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

const probeFixtureLabel = "org.zenoglyphs.fleet.probe-fixture"

type probeFixtureDigest struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

type probeFixtureKernel struct {
	Limits                               map[string]int64
	Effective, Bounding, NoNewPrivileges string
	RootReadOnly                         bool
}

type probeFixtureContainer struct{ id, name string }
type probeFixtureLab struct {
	kernel           []probeFixtureKernel
	t                *testing.T
	ctx              context.Context
	token, image     string
	containers       []probeFixtureContainer
	cleanupAttempted bool
}

func probeFixtureFile(t *testing.T, path string, limit int64) probeFixtureDigest {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		t.Fatal("owned fixture artifact is missing, nonregular or outside its byte budget")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("opening owned fixture artifact failed")
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil || n != info.Size() {
		t.Fatal("reading owned fixture artifact failed")
	}
	return probeFixtureDigest{hex.EncodeToString(h.Sum(nil)), n, uint32(info.Mode().Perm())}
}

func (l *probeFixtureLab) call(ctx context.Context, args ...string) ([]byte, error) {
	return measurementCommand(ctx, args...)
}

func (l *probeFixtureLab) require(args ...string) []byte {
	l.t.Helper()
	raw, err := l.call(l.ctx, args...)
	if err != nil {
		l.t.Fatal("owned Docker fixture operation failed:", measurementReportReason(err))
	}
	return raw
}

func (l *probeFixtureLab) labels(ctx context.Context, kind, id string) (map[string]string, error) {
	raw, err := l.call(ctx, kind, "inspect", "--format", "{{json .Config.Labels}}", id)
	if err != nil {
		return nil, err
	}
	var labels map[string]string
	if json.Unmarshal(raw, &labels) != nil || labels[probeFixtureLabel] != l.token {
		return nil, errors.New("fixture resource ownership is unknown")
	}
	return labels, nil
}

func (l *probeFixtureLab) cleanup() (map[string]any, error) {
	l.cleanupAttempted = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	removed := 0
	var result error
	for _, c := range l.containers {
		if _, err := l.labels(ctx, "container", c.id); err != nil {
			result = errors.Join(result, err)
			continue
		}
		raw, err := l.call(ctx, "container", "inspect", "--format", "{{.Name}}", c.id)
		if err != nil || strings.TrimSpace(string(raw)) != "/"+c.name {
			result = errors.Join(result, errors.New("owned container identity changed"))
			continue
		}
		raw, err = l.call(ctx, "rm", "--force", c.id)
		if err != nil || strings.TrimSpace(string(raw)) != c.id {
			result = errors.Join(result, errors.New("owned container removal is unknown"))
			continue
		}
		removed++
	}
	remaining, err := l.call(ctx, "container", "ls", "--all", "--quiet", "--filter", "label="+probeFixtureLabel+"="+l.token)
	result = errors.Join(result, err)
	if len(strings.Fields(string(remaining))) != 0 {
		result = errors.Join(result, errors.New("owned containers remain"))
	}
	imageRemoved := false
	if l.image != "" && result == nil {
		if _, err := l.labels(ctx, "image", l.image); err != nil {
			result = errors.Join(result, err)
		} else {
			_, err = l.call(ctx, "image", "rm", l.image)
			result = errors.Join(result, err)
			imageRemoved = err == nil
		}
	}
	images, err := l.call(ctx, "image", "ls", "--all", "--quiet", "--filter", "label="+probeFixtureLabel+"="+l.token)
	result = errors.Join(result, err)
	if len(strings.Fields(string(images))) != 0 {
		result = errors.Join(result, errors.New("owned images remain"))
	}
	return map[string]any{"createdContainers": len(l.containers), "removedContainers": removed,
		"remainingOwnedContainers": len(strings.Fields(string(remaining))), "imageRemoved": imageRemoved,
		"remainingOwnedImages": len(strings.Fields(string(images))), "unownedContainerOrImageMutationCommands": 0}, result
}

func (l *probeFixtureLab) create(role string) string {
	l.t.Helper()
	name := "fleet-probe-fixture-" + l.token + "-" + role
	raw := l.require("create", "--name", name, "--label", probeFixtureLabel+"="+l.token,
		"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--cpus", "1", "--memory", "384m", "--memory-swap", "384m", "--pids-limit", "64",
		"--tmpfs", "/tmp:rw,exec,nodev,nosuid,size=128m", "--tmpfs", "/data:rw,noexec,nodev,nosuid,size=16m", l.image)
	id := strings.TrimSpace(string(raw))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		l.t.Fatal("owned container creation returned an invalid identity")
	}
	l.containers = append(l.containers, probeFixtureContainer{id, name})
	if _, err := l.labels(l.ctx, "container", id); err != nil {
		l.t.Fatal("owned container labels were not preserved")
	}
	var observed struct {
		ID          string `json:"Id"`
		Name, Image string
		HostConfig  struct {
			NetworkMode                  string
			ReadonlyRootfs               bool
			Memory, MemorySwap, NanoCPUs int64
			PidsLimit                    *int64
			CapDrop, SecurityOpt         []string
		}
	}
	if json.Unmarshal(l.require("container", "inspect", "--format", "{{json .}}", id), &observed) != nil {
		l.t.Fatal("owned container constraints could not be inspected")
	}
	h := observed.HostConfig
	if observed.ID != id || observed.Name != "/"+name || observed.Image != l.image || h.NetworkMode != "none" || !h.ReadonlyRootfs ||
		h.Memory != 384<<20 || h.MemorySwap != h.Memory || h.NanoCPUs != 1_000_000_000 || h.PidsLimit == nil || *h.PidsLimit != 64 {
		l.t.Fatal("actual owned container constraints differ from the declared fixture")
	}
	contains := func(list []string, value string) bool {
		for _, item := range list {
			if item == value {
				return true
			}
		}
		return false
	}
	if !(contains(h.CapDrop, "ALL") || contains(h.CapDrop, "CAP_ALL")) || !(contains(h.SecurityOpt, "no-new-privileges:true") || contains(h.SecurityOpt, "no-new-privileges")) {
		l.t.Fatal("owned container privilege constraints differ")
	}
	l.require("start", id)
	var kernel probeFixtureKernel
	if json.Unmarshal(l.require("exec", id, "/bin/chmod", "runtime-limits"), &kernel) != nil ||
		kernel.Limits["memoryBytes"] != 384<<20 || kernel.Limits["swapBytes"] != 0 || kernel.Limits["pids"] != 64 ||
		kernel.Limits["cpuQuota"] <= 0 || kernel.Limits["cpuQuota"] != kernel.Limits["cpuPeriod"] ||
		kernel.Effective != "0000000000000000" || kernel.Bounding != "0000000000000000" || kernel.NoNewPrivileges != "1" || !kernel.RootReadOnly {
		l.t.Fatal("actual kernel cgroup, capability or mount constraints differ")
	}
	l.kernel = append(l.kernel, kernel)
	return id
}

func (l *probeFixtureLab) digest(t *testing.T, id, operation string) probeFixtureDigest {
	t.Helper()
	var got probeFixtureDigest
	raw, err := l.call(l.ctx, "exec", id, "/bin/chmod", operation)
	if err != nil || json.Unmarshal(raw, &got) != nil || len(got.SHA256) != 64 || got.Bytes < 1 {
		t.Fatal("owned container file digest is unavailable")
	}
	return got
}

type probeFixtureDB struct {
	path                       string
	digest                     probeFixtureDigest
	pageSize, pages, free      int64
	indexed, momentum, derived int64
	rows                       map[string]int64
}

func probeFixtureDatabase(t *testing.T, dir string, follower bool) probeFixtureDB {
	t.Helper()
	path := filepath.Join(dir, "zvm.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal("opening synthetic SQLite fixture failed")
	}
	statements := []string{"PRAGMA page_size=4096", "PRAGMA journal_mode=DELETE",
		"CREATE TABLE blocks(number INTEGER NOT NULL)", "CREATE TABLE momentums(height INTEGER NOT NULL,l2_number INTEGER NOT NULL)",
		"CREATE TABLE payload(id INTEGER PRIMARY KEY,value BLOB NOT NULL)", "CREATE INDEX payload_lookup ON payload(value)",
		"INSERT INTO payload VALUES(1,zeroblob(37)),(2,zeroblob(109))"}
	fixture := probeFixtureDB{path: path, indexed: 11, momentum: 23, derived: 19, rows: map[string]int64{"blocks": 2, "momentums": 2, "payload": 2}}
	if follower {
		fixture.indexed, fixture.momentum, fixture.derived = 4, 5, 3
		fixture.rows["blocks"], fixture.rows["momentums"] = 1, 1
		statements = append(statements, "INSERT INTO blocks VALUES(4)", "INSERT INTO momentums VALUES(5,3)")
	} else {
		statements = append(statements, "INSERT INTO blocks VALUES(7),(11)", "INSERT INTO momentums VALUES(17,13),(23,19)")
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal("creating synthetic SQLite fixture failed")
		}
	}
	for pragma, dst := range map[string]*int64{"page_size": &fixture.pageSize, "page_count": &fixture.pages, "freelist_count": &fixture.free} {
		if err := db.QueryRow("PRAGMA " + pragma).Scan(dst); err != nil {
			db.Close()
			t.Fatal("reading independent SQLite fixture denominator failed")
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal("closing synthetic SQLite fixture failed")
	}
	fixture.digest = probeFixtureFile(t, path, 1<<20)
	return fixture
}

func requireProbeFixtureReport(t *testing.T, got *bench.StorageReport, fixture probeFixtureDB, media bool) {
	t.Helper()
	if got == nil || got.Path != "/data/zvm.db" || got.FileBytes != fixture.digest.Bytes || got.WALBytes != 0 ||
		got.PageSize != fixture.pageSize || got.Pages != fixture.pages || got.FreePages != fixture.free || got.At <= 0 || got.TookMS < 0 {
		t.Fatal("real probe differs from independent synthetic file and SQLite denominators")
	}
	if got.Heads["indexedBlock"] != fixture.indexed || got.Heads["derivedMomentum"] != fixture.momentum || got.Heads["derivedBlock"] != fixture.derived {
		t.Fatal("real probe lost the distinct synthetic primary/follower heads")
	}
	rows := map[string]int64{}
	for _, table := range got.Tables {
		rows[table.Name] = table.Rows
		if table.Bytes < 0 || table.IndexBytes < 0 || table.IndexBytes > table.Bytes {
			t.Fatal("invalid real probe table accounting")
		}
	}
	if len(rows) != len(fixture.rows) {
		t.Fatal("real probe table set differs")
	}
	for name, count := range fixture.rows {
		if rows[name] != count {
			t.Fatal("real probe row denominator differs")
		}
	}
	if len(got.Objects) == 0 {
		t.Fatal("real SQLite dbstat objects are unavailable")
	}
	if media {
		if got.Media == nil || got.Media.Path != "/data/media" || got.Media.Files != 2 || got.Media.Bytes != 1025 {
			t.Fatal("real media capture differs from the owned files")
		}
	} else if got.Media != nil {
		t.Fatal("follower probe invented a media observation")
	}
}

func TestProbeOwnedDockerSQLite(t *testing.T) {
	if os.Getenv("FLEET_OWNED_DOCKER") != "1" || os.Getenv("GITHUB_ACTIONS") != "true" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("the owned Linux/amd64 hosted Docker fixture must be explicitly selected")
	}
	digits := regexp.MustCompile(`^[0-9]+$`)
	if !digits.MatchString(os.Getenv("GITHUB_RUN_ID")) || !digits.MatchString(os.Getenv("GITHUB_RUN_ATTEMPT")) {
		t.Fatal("hosted fixture ownership is unavailable")
	}
	root := filepath.Join(os.Getenv("RUNNER_TEMP"), "fleet-probe-"+os.Getenv("GITHUB_RUN_ID")+"-"+os.Getenv("GITHUB_RUN_ATTEMPT"))
	probe, helper := filepath.Join(root, "zvm-probe"), filepath.Join(root, "helper")
	probeDigest, helperDigest := probeFixtureFile(t, probe, 96<<20), probeFixtureFile(t, helper, 8<<20)
	if probeDigest.Bytes < 1<<20 || probeDigest.Mode&0o111 != 0 {
		t.Fatal("the real probe fixture must start as a nonexecutable transferred artifact")
	}
	info, err := buildinfo.ReadFile(probe)
	if err != nil || info.Path != "github.com/Zenoglyphs-VM/zvm-node/cmd/zvm-node" {
		t.Fatal("fixture is not the full pinned Node command")
	}
	settings := map[string]string{}
	for _, value := range info.Settings {
		settings[value.Key] = value.Value
	}
	if settings["GOOS"] != "linux" || settings["GOARCH"] != "amd64" || settings["CGO_ENABLED"] != "0" || settings["-trimpath"] != "true" {
		t.Fatal("probe build settings differ from the declared static Linux fixture")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("Docker is a required fixture prerequisite")
	}
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, "config.json"), []byte("{\"auths\":{}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"DOCKER_CONFIG": config, "DOCKER_HOST": "unix:///var/run/docker.sock", "DOCKER_CONTEXT": "", "DOCKER_TLS_VERIFY": "", "DOCKER_CERT_PATH": "", "BUILDX_BUILDER": "", "DOCKER_BUILDKIT": "1"} {
		t.Setenv(key, value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal("fixture ownership token unavailable")
	}
	lab := &probeFixtureLab{t: t, ctx: ctx, token: hex.EncodeToString(token[:])}
	t.Cleanup(func() {
		if !lab.cleanupAttempted {
			if _, err := lab.cleanup(); err != nil {
				t.Error("owned fixture cleanup is UNKNOWN")
			}
		}
	})
	if strings.TrimSpace(string(lab.require("version", "--format", "{{.Server.Os}}/{{.Server.Arch}}"))) != "linux/amd64" {
		t.Fatal("fixture requires a local Linux/amd64 Docker daemon")
	}
	version := strings.TrimSpace(string(lab.require("version", "--format", "{{.Server.Version}}")))
	contextDir := t.TempDir()
	in, err := os.Open(helper)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(filepath.Join(contextDir, "helper"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		in.Close()
		t.Fatal(err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(in, (8<<20)+1))
	closeErr := errors.Join(in.Close(), out.Close())
	if copyErr != nil || closeErr != nil || n != helperDigest.Bytes {
		t.Fatal("bounded helper context copy failed")
	}
	dockerfile, err := os.ReadFile(filepath.Join("..", "testdata", "probe-fixture", "Dockerfile"))
	if err != nil || len(dockerfile) > 1024 {
		t.Fatal("owned scratch-image recipe unavailable")
	}
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), dockerfile, 0o600); err != nil {
		t.Fatal(err)
	}
	iid := filepath.Join(t.TempDir(), "image-id")
	lab.require("build", "--quiet", "--pull=false", "--network=none", "--iidfile", iid, "--label", probeFixtureLabel+"="+lab.token, contextDir)
	image, err := os.ReadFile(iid)
	if err != nil {
		t.Fatal("owned image identity unavailable")
	}
	lab.image = strings.TrimSpace(string(image))
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(lab.image) {
		t.Fatal("owned image identity invalid")
	}
	if _, err := lab.labels(ctx, "image", lab.image); err != nil {
		t.Fatal("owned image labels differ")
	}
	primary, follower := lab.create("primary"), lab.create("follower")
	primaryDB, followerDB := probeFixtureDatabase(t, t.TempDir(), false), probeFixtureDatabase(t, t.TempDir(), true)
	for id, fixture := range map[string]probeFixtureDB{primary: primaryDB, follower: followerDB} {
		lab.require("cp", fixture.path, id+":/data/zvm.db")
	}
	mediaDir := t.TempDir()
	if os.WriteFile(filepath.Join(mediaDir, "empty"), nil, 0o600) != nil || os.WriteFile(filepath.Join(mediaDir, "payload.bin"), make([]byte, 1025), 0o600) != nil {
		t.Fatal("owned media fixture creation failed")
	}
	lab.require("cp", mediaDir, primary+":/data/media")
	e := &Env{Probe: probe, Names: Names{Node: primary, NodeB: follower}, running: map[string]bool{primary: true, follower: true}}
	if err := e.installProbe(ctx); err != nil {
		t.Fatal("actual probe installation failed:", measurementReportReason(err))
	}
	var primaryObserved, followerObserved *bench.StorageReport
	var primaryDBAfter, followerDBAfter probeFixtureDigest
	observedCopies := map[string]probeFixtureDigest{}
	for role, id := range map[string]string{"primary": primary, "follower": follower} {
		t.Run("installed-"+role, func(t *testing.T) {
			got := lab.digest(t, id, "probe-digest")
			observedCopies[role] = got
			if got.SHA256 != probeDigest.SHA256 || got.Bytes != probeDigest.Bytes || got.Mode&0o111 != 0o111 {
				t.Fatal("actual probe copy/chmod bytes or mode differ")
			}
		})
	}
	t.Run("sqlite-primary-with-media", func(t *testing.T) {
		got, file, pages, err := e.probeObserved(ctx, primary, true)
		primaryObserved = got
		if err != nil || !file || !pages {
			t.Fatal("actual primary SQLite observation unavailable")
		}
		requireProbeFixtureReport(t, got, primaryDB, true)
		after := lab.digest(t, primary, "db-digest")
		primaryDBAfter = after
		if after.SHA256 != primaryDB.digest.SHA256 || after.Bytes != primaryDB.digest.Bytes {
			t.Fatal("read-only primary probe changed the fixture database")
		}
	})
	t.Run("sqlite-follower-without-media", func(t *testing.T) {
		got, file, pages, err := e.probeObserved(ctx, follower, false)
		followerObserved = got
		if err != nil || !file || !pages {
			t.Fatal("actual follower SQLite observation unavailable")
		}
		requireProbeFixtureReport(t, got, followerDB, false)
		after := lab.digest(t, follower, "db-digest")
		followerDBAfter = after
		if after.SHA256 != followerDB.digest.SHA256 || after.Bytes != followerDB.digest.Bytes {
			t.Fatal("read-only follower probe changed the fixture database")
		}
	})
	t.Run("caller-canceled", func(t *testing.T) {
		canceled, stop := context.WithCancel(ctx)
		stop()
		got, file, pages, err := e.probeObserved(canceled, primary, true)
		if !errors.Is(err, context.Canceled) || got != nil || file || pages {
			t.Fatal("pre-dispatch cancellation invented an observation")
		}
	})
	lab.require("stop", "--time", "1", follower)
	t.Run("stale-running-follower", func(t *testing.T) {
		got, file, pages, err := e.probeObserved(ctx, follower, false)
		var exited *exec.ExitError
		if got != nil || file || pages || !errors.As(err, &exited) || exited.ExitCode() == 0 {
			t.Fatal("stopped Docker exec invented an observation")
		}
	})
	e.running[follower] = false
	t.Run("absent-follower", func(t *testing.T) {
		got, file, pages, err := e.probeObserved(ctx, follower, false)
		if err != nil || got != nil || file || pages {
			t.Fatal("absent follower invented an observation")
		}
	})
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	if os.WriteFile(corrupt, []byte("synthetic invalid SQLite database\n"), 0o600) != nil {
		t.Fatal("owned corrupt database fixture creation failed")
	}
	lab.require("cp", corrupt, primary+":/data/zvm.db")
	t.Run("corrupt-database", func(t *testing.T) {
		got, file, pages, err := e.probeObserved(ctx, primary, true)
		var exited *exec.ExitError
		if got != nil || file || pages || !errors.As(err, &exited) || exited.ExitCode() == 0 {
			t.Fatal("actual SQLite failure invented an observation")
		}
	})
	cleanup, err := lab.cleanup()
	if err != nil {
		t.Fatal("owned fixture cleanup is UNKNOWN")
	}
	if t.Failed() {
		return
	}
	jsonRecord, err := json.Marshal(map[string]any{"schema": 1, "fleetSource": os.Getenv("GITHUB_SHA"), "nodeProbeSource": "ae90b945b0ea95887c31cfb2f19e0b88504186d8",
		"probe": probeDigest, "helper": helperDigest, "probeGoVersion": info.GoVersion, "buildSettings": settings, "dockerServerVersion": version,
		"kernelConstraints":         lab.kernel,
		"observedProbeReports":      map[string]*bench.StorageReport{"primary": primaryObserved, "follower": followerObserved},
		"observedTransferredProbes": observedCopies,
		"observedDatabaseDigests":   map[string]probeFixtureDigest{"primary": primaryDBAfter, "follower": followerDBAfter},
		"containers":                2, "perContainerMemoryBytes": 384 << 20, "perContainerCPUs": 1, "perContainerPIDs": 64, "network": "none", "rootReadOnly": true, "kernelLimitsVerified": true,
		"primary":    map[string]any{"sha256": primaryDB.digest.SHA256, "fileBytes": primaryDB.digest.Bytes, "pageSize": primaryDB.pageSize, "pages": primaryDB.pages, "freePages": primaryDB.free, "rows": primaryDB.rows},
		"follower":   map[string]any{"sha256": followerDB.digest.SHA256, "fileBytes": followerDB.digest.Bytes, "pageSize": followerDB.pageSize, "pages": followerDB.pages, "freePages": followerDB.free, "rows": followerDB.rows},
		"mediaFiles": 2, "mediaBytes": 1025, "readOnlyDatabaseHashesPreserved": true, "cleanup": cleanup,
		"slowLargeCopyQualified": false, "WindowsQualified": false, "fullServiceOrChainQualified": false, "performanceBenchmark": false})
	if err != nil {
		t.Fatal(fmt.Errorf("encoding owned fixture evidence: %w", err))
	}
	t.Logf("FLEET_PROBE_FIXTURE %s", jsonRecord)
}
