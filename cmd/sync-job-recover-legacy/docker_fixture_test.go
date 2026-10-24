//go:build qa_p1_bi_docker

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestDockerStoppedEvidenceUsesCompleteOwnedFixtureBoundary(t *testing.T) {
	host, image := os.Getenv("UIP_BI_TEST_DOCKER_HOST"), os.Getenv("UIP_BI_TEST_DOCKER_IMAGE")
	if !strings.HasPrefix(host, "unix://") || !strings.HasPrefix(image, "sha256:") {
		t.Fatal("explicit local Unix Docker host and cached immutable fixture image required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	project := "qa-p1-bi-stop-" + time.Now().UTC().Format("20060102150405.000000000")
	t.Logf("Owned fixture project: %s", project)
	command := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", append([]string{"--host", host}, args...)...).Output()
	}
	create := func() string {
		t.Helper()
		raw, err := command("create", "--network", "none", "--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service=data-analysis-aggregation-worker", "--entrypoint", "/bin/sh", image, "-c", "sleep 120")
		if err != nil {
			t.Fatal("owned fixture create failed")
		}
		id := strings.TrimSpace(string(raw))
		if len(id) != 64 {
			t.Fatal("invalid fixture container identity")
		}
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			raw, e := exec.CommandContext(cleanupCtx, "docker", "--host", host, "inspect", id).Output()
			if e != nil {
				t.Errorf("owned fixture inspection cleanup failed")
				return
			}
			var rows []inspectRecord
			if e = json.Unmarshal(raw, &rows); e != nil || len(rows) != 1 || rows[0].Config.Labels["com.docker.compose.project"] != project {
				t.Errorf("fixture cleanup refuses unknown owner")
				return
			}
			if e = exec.CommandContext(cleanupCtx, "docker", "--host", host, "rm", "--force", id).Run(); e != nil {
				t.Errorf("owned fixture removal failed")
			}
		})
		return id
	}
	first := create()
	expected := map[string]string{first: image}
	if _, err := stoppedEvidenceOnDockerHost(ctx, host, project, expected); err != nil {
		t.Fatal(err)
	}
	if _, err := command("start", first); err != nil {
		t.Fatal("owned fixture start failed")
	}
	if _, err := stoppedEvidenceOnDockerHost(ctx, host, project, expected); err == nil {
		t.Fatal("real running worker accepted as stopped")
	}
	if _, err := command("stop", "--time", "1", first); err != nil {
		t.Fatal("owned fixture stop failed")
	}
	if _, err := stoppedEvidenceOnDockerHost(ctx, host, project, expected); err != nil {
		t.Fatal(err)
	}
	second := create()
	if _, err := stoppedEvidenceOnDockerHost(ctx, host, project, expected); err == nil {
		t.Fatal("partial deployment boundary accepted")
	}
	expected[second] = image
	if _, err := stoppedEvidenceOnDockerHost(ctx, host, project, expected); err != nil {
		t.Fatal(err)
	}
	expected[first] = "sha256:" + strings.Repeat("0", 64)
	if _, err := stoppedEvidenceOnDockerHost(ctx, host, project, expected); err == nil {
		t.Fatal("changed immutable image accepted")
	}
}
