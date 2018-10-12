/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/server"
)

func TestSandboxCleanRemove(t *testing.T) {
	ctx := context.Background()
	t.Logf("Create a sandbox")
	sbConfig := PodSandboxConfig("sandbox", "clean-remove")
	sb, err := runtimeService.RunPodSandbox(sbConfig)
	require.NoError(t, err)
	defer func() {
		// Make sure the sandbox is cleaned up in any case.
		runtimeService.StopPodSandbox(sb)
		runtimeService.RemovePodSandbox(sb)
	}()

	t.Logf("Should not be able to remove the sandbox when it's still running")
	assert.Error(t, runtimeService.RemovePodSandbox(sb))

	t.Logf("Delete sandbox task from containerd")
	cntr, err := containerdClient.LoadContainer(ctx, sb)
	require.NoError(t, err)
	task, err := cntr.Task(ctx, nil)
	require.NoError(t, err)
	_, err = task.Delete(ctx, containerd.WithProcessKill)
	require.NoError(t, err)

	t.Logf("Sandbox state should be NOTREADY")
	assert.NoError(t, Eventually(func() (bool, error) {
		status, err := runtimeService.PodSandboxStatus(sb)
		if err != nil {
			return false, err
		}
		return status.GetState() == runtime.PodSandboxState_SANDBOX_NOTREADY, nil
	}, time.Second, 30*time.Second), "sandbox state should become NOTREADY")

	t.Logf("Should not be able to remove the sandbox when netns is not closed")
	assert.Error(t, runtimeService.RemovePodSandbox(sb))

	t.Logf("Should be able to remove the sandbox after properly stopped")
	assert.NoError(t, runtimeService.StopPodSandbox(sb))
	assert.NoError(t, runtimeService.RemovePodSandbox(sb))
}

func TestSandboxRemoveWithoutIPLeakage(t *testing.T) {
	ctx := context.Background()
	const hostLocalCheckpointDir = "/var/lib/cni"

	t.Logf("Make sure host-local ipam is in use")
	config, err := CRIConfig()
	require.NoError(t, err)
	fs, err := ioutil.ReadDir(config.NetworkPluginConfDir)
	require.NoError(t, err)
	require.NotEmpty(t, fs)
	f := filepath.Join(config.NetworkPluginConfDir, fs[0].Name())
	cniConfig, err := ioutil.ReadFile(f)
	require.NoError(t, err)
	if !strings.Contains(string(cniConfig), "host-local") {
		t.Skip("host-local ipam is not in use")
	}

	t.Logf("Create a sandbox")
	sbConfig := PodSandboxConfig("sandbox", "remove-without-ip-leakage")
	sb, err := runtimeService.RunPodSandbox(sbConfig)
	require.NoError(t, err)
	defer func() {
		// Make sure the sandbox is cleaned up in any case.
		runtimeService.StopPodSandbox(sb)
		runtimeService.RemovePodSandbox(sb)
	}()

	t.Logf("Get pod information")
	client, err := RawRuntimeClient()
	require.NoError(t, err)
	resp, err := client.PodSandboxStatus(ctx, &runtime.PodSandboxStatusRequest{
		PodSandboxId: sb,
		Verbose:      true,
	})
	require.NoError(t, err)
	status := resp.GetStatus()
	info := resp.GetInfo()
	ip := status.GetNetwork().GetIp()
	require.NotEmpty(t, ip)
	var sbInfo server.SandboxInfo
	require.NoError(t, json.Unmarshal([]byte(info["info"]), &sbInfo))
	require.NotNil(t, sbInfo.RuntimeSpec.Linux)
	var netNS string
	for _, n := range sbInfo.RuntimeSpec.Linux.Namespaces {
		if n.Type == runtimespec.NetworkNamespace {
			netNS = n.Path
		}
	}
	require.NotEmpty(t, netNS, "network namespace should be set")

	t.Logf("Should be able to find the pod ip in host-local checkpoint")
	checkIP := func(ip string) bool {
		found := false
		filepath.Walk(hostLocalCheckpointDir, func(_ string, info os.FileInfo, _ error) error {
			if info != nil && info.Name() == ip {
				found = true
			}
			return nil
		})
		return found
	}
	require.True(t, checkIP(ip))

	t.Logf("Kill sandbox container")
	require.NoError(t, KillPid(int(sbInfo.Pid)))

	t.Logf("Unmount network namespace")
	// The umount will take effect after containerd is stopped.
	require.NoError(t, unix.Unmount(netNS, unix.MNT_DETACH))

	t.Logf("Restart containerd")
	RestartContainerd(t)

	t.Logf("Sandbox state should be NOTREADY")
	assert.NoError(t, Eventually(func() (bool, error) {
		status, err := runtimeService.PodSandboxStatus(sb)
		if err != nil {
			return false, err
		}
		return status.GetState() == runtime.PodSandboxState_SANDBOX_NOTREADY, nil
	}, time.Second, 30*time.Second), "sandbox state should become NOTREADY")

	t.Logf("Network namespace should have been removed")
	_, err = os.Stat(netNS)
	assert.True(t, os.IsNotExist(err))

	t.Logf("Should still be able to find the pod ip in host-local checkpoint")
	assert.True(t, checkIP(ip))

	t.Logf("Should be able to remove the sandbox after properly stopped")
	assert.NoError(t, runtimeService.StopPodSandbox(sb))
	assert.NoError(t, runtimeService.RemovePodSandbox(sb))

	t.Logf("Should not be able to find the pod ip in host-local checkpoint")
	assert.False(t, checkIP(ip))
}
