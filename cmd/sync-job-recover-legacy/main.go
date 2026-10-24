// sync-job-recover-legacy 默认预览所选的租约机制引入前任务。恢复
// 是平台部署锁保护下的 root 维护操作。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

type workerFact struct {
	ID         string
	Image      string
	Running    bool
	Paused     bool
	Restarting bool
}
type inspectRecord struct {
	ID    string `json:"Id"`
	Image string
	State struct {
		Running    bool
		Paused     bool
		Restarting bool
	}
	Config struct{ Labels map[string]string }
}

func stoppedEvidence(ctx context.Context, project string, expected map[string]string) ([]byte, error) {
	return stoppedEvidenceOnDockerHost(ctx, "unix:///var/run/docker.sock", project, expected)
}

func stoppedEvidenceOnDockerHost(ctx context.Context, dockerHost, project string, expected map[string]string) ([]byte, error) {
	if !strings.HasPrefix(dockerHost, "unix://") {
		return nil, errors.New("local Unix Docker host required")
	}
	if project == "" || len(expected) == 0 {
		return nil, errors.New("complete expected stopped container IDs and immutable image IDs required")
	}
	// 禁止将 Docker 检查的环境或配置序列化到输出中。
	cmd := exec.CommandContext(ctx, "docker", "--host", dockerHost, "ps", "--all", "--no-trunc", "--quiet", "--filter", "label=com.docker.compose.project="+project, "--filter", "label=com.docker.compose.service=data-analysis-aggregation-worker")
	raw, err := cmd.Output()
	if err != nil {
		return nil, errors.New("cannot enumerate worker deployment boundary")
	}
	ids := strings.Fields(string(raw))
	sort.Strings(ids)
	if len(ids) != len(expected) {
		return nil, errors.New("worker container boundary does not match reviewed identities")
	}
	facts := make([]workerFact, 0, len(ids))
	for _, id := range ids {
		image, found := expected[id]
		if !found {
			return nil, errors.New("unexpected worker container")
		}
		raw, err = exec.CommandContext(ctx, "docker", "--host", dockerHost, "inspect", id).Output()
		if err != nil {
			return nil, errors.New("cannot inspect stopped worker")
		}
		var rows []inspectRecord
		if err = json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 {
			return nil, errors.New("invalid worker inspection")
		}
		fact, err := checkedStoppedWorker(project, id, image, rows[0])
		if err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return json.Marshal(struct {
		Project string
		Service string
		Workers []workerFact
	}{project, "data-analysis-aggregation-worker", facts})
}

func checkedStoppedWorker(project, id, image string, r inspectRecord) (workerFact, error) {
	if r.ID != id || r.Image != image || r.Config.Labels["com.docker.compose.project"] != project || r.Config.Labels["com.docker.compose.service"] != "data-analysis-aggregation-worker" {
		return workerFact{}, errors.New("worker identity/image/compose boundary changed")
	}
	if r.State.Running || r.State.Paused || r.State.Restarting {
		return workerFact{}, errors.New("all old aggregation workers must be stopped under controlled deployment maintenance")
	}
	return workerFact{r.ID, r.Image, r.State.Running, r.State.Paused, r.State.Restarting}, nil
}

func deploymentLock(root string) (*os.File, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != resolved {
		return nil, errors.New("canonical absolute deployment root required")
	}
	runtimePath := filepath.Join(root, "runtime")
	resolvedRuntime, err := filepath.EvalSymlinks(runtimePath)
	if err != nil || resolvedRuntime != runtimePath {
		return nil, errors.New("canonical deployment runtime directory required")
	}
	path := filepath.Join(root, "runtime", ".deploy.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("invalid deployment lock")
	}
	if info.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, errors.New("deployment lock must not be writable by other users")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("controlled deployment maintenance lock is busy")
	}
	return f, nil
}

func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("sync-job-recover-legacy", flag.ContinueOnError)
	apply := f.Bool("apply", false, "commit reviewed recovery (default is read-only preview)")
	tenant := f.String("tenant-id", "", "exact tenant")
	jobs := f.String("job-ids", "", "comma-separated exact legacy RUNNING IDs")
	operator := f.String("operator", "", "audited human operator")
	root := f.String("deployment-root", "", "canonical deployment root for runtime/.deploy.lock")
	project := f.String("compose-project", "", "reviewed Compose project")
	workers := f.String("expected-workers", "", "comma-separated full containerID@sha256:imageID identities from controlled stop")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *tenant == "" || *jobs == "" {
		return errors.New("tenant-id and explicit job-ids required")
	}
	if *apply && os.Geteuid() != 0 {
		return errors.New("apply requires root")
	}
	r, err := aggregation.NewRunner(os.Getenv("DASHBOARD_MYSQL_DSN"), *tenant, nil)
	if err != nil {
		return errors.New("cannot open aggregation database")
	}
	ids := strings.Split(*jobs, ",")
	preview, err := r.PreviewLegacySyncJobs(ctx, ids)
	if err != nil {
		return errors.New("selected legacy job state/version/provenance does not match")
	}
	if !*apply {
		rows := make([]map[string]any, 0, len(preview))
		for _, j := range preview {
			rows = append(rows, map[string]any{"id": j.ID, "tenant_id": j.TenantID, "status": j.Status, "lease_generation": j.LeaseGeneration, "source_id": j.SourceID})
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"dry_run": true, "jobs": rows, "requires": "root, deployment lock, complete stopped Compose worker identities, operator"})
	}
	if os.Geteuid() != 0 || *operator == "" || *root == "" || *project == "" {
		return errors.New("apply requires root, operator, deployment root and project")
	}
	if host := os.Getenv("DOCKER_HOST"); host != "" && !strings.HasPrefix(host, "unix://") {
		return errors.New("remote Docker host cannot prove local deployment maintenance")
	}
	lock, err := deploymentLock(*root)
	if err != nil {
		return err
	}
	defer lock.Close()
	expected := map[string]string{}
	for _, raw := range strings.Split(*workers, ",") {
		parts := strings.Split(raw, "@")
		if len(parts) != 2 || len(parts[0]) != 64 || !strings.HasPrefix(parts[1], "sha256:") || len(parts[1]) != 71 {
			return errors.New("full immutable worker identities required")
		}
		if _, dup := expected[parts[0]]; dup {
			return errors.New("duplicate worker identity")
		}
		expected[parts[0]] = parts[1]
	}
	verifier := func(ctx context.Context) ([]byte, error) { return stoppedEvidence(ctx, *project, expected) }
	if err = r.RecoverLegacySyncJobs(ctx, aggregation.LegacyRecoveryInput{JobIDs: ids, Operator: *operator, VerifyStopped: verifier}); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"recovered": ids, "tenant_id": *tenant, "operator": *operator, "audit": "sync_job_recovery_audit", "status": "QUEUED"})
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "legacy sync recovery failed:", err)
		os.Exit(1)
	}
}
