package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyRecoveryStoppedWorkerProofChecksIdentityImageAndLiveness(t *testing.T) {
	good := inspectRecord{ID: "container", Image: "sha256:image"}
	good.Config.Labels = map[string]string{"com.docker.compose.project": "uip", "com.docker.compose.service": "data-analysis-aggregation-worker"}
	if _, err := checkedStoppedWorker("uip", "container", "sha256:image", good); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"running", "paused", "restarting", "image", "id", "project", "service"} {
		t.Run(scenario, func(t *testing.T) {
			bad := good
			bad.Config.Labels = map[string]string{"com.docker.compose.project": "uip", "com.docker.compose.service": "data-analysis-aggregation-worker"}
			switch scenario {
			case "running":
				bad.State.Running = true
			case "paused":
				bad.State.Paused = true
			case "restarting":
				bad.State.Restarting = true
			case "image":
				bad.Image = "other"
			case "id":
				bad.ID = "other"
			case "project":
				bad.Config.Labels["com.docker.compose.project"] = "other"
			case "service":
				bad.Config.Labels["com.docker.compose.service"] = "api"
			}
			if _, err := checkedStoppedWorker("uip", "container", "sha256:image", bad); err == nil {
				t.Fatal("unproven stopped-worker fact accepted")
			}
		})
	}
	if _, err := stoppedEvidence(context.Background(), "uip", nil); err == nil {
		t.Fatal("empty worker boundary accepted")
	}
}

func TestLegacyRecoveryHoldsExistingDeploymentLockAndRejectsSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := deploymentLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := deploymentLock(root); err == nil {
		second.Close()
		t.Fatal("parallel maintenance acquired deployment lock")
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "runtime", ".deploy.lock")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "outside"), path); err != nil {
		t.Fatal(err)
	}
	if lock, err = deploymentLock(root); err == nil {
		lock.Close()
		t.Fatal("symlink deployment lock accepted")
	}
}
