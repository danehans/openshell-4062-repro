// Reproduces NVIDIA/OpenShell#4062 using only the public Go SDK.
//
// Two logical policy writers read the same sandbox snapshot (resource version
// R and policy P). Writer A commits a new policy revision using R. Writer B
// then submits a different policy using the same, now-stale R.
//
// Expected (correct) behavior: B is rejected with gRPC Aborted, because A's
// committed revision must advance the sandbox resource version.
// Bug: B is accepted and silently replaces A's revision.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"
	"google.golang.org/grpc/codes"
)

const workspace = "default"

func main() {
	address := flag.String("address", "localhost:8080", "Gateway endpoint")
	tlsDir := flag.String("tls-dir", "", "Directory containing ca.crt, tls.crt, tls.key (omit for plaintext)")
	image := flag.String("image", "", "Sandbox image the Gateway can run (empty = Gateway default)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	os.Exit(run(ctx, *address, *tlsDir, *image))
}

func run(ctx context.Context, address, tlsDir, image string) int {
	cfg := v1.Config{Address: address}
	if tlsDir != "" {
		cfg.TLS = &types.TLSConfig{
			CAFile:   filepath.Join(tlsDir, "ca.crt"),
			CertFile: filepath.Join(tlsDir, "tls.crt"),
			KeyFile:  filepath.Join(tlsDir, "tls.key"),
		}
	}
	client, err := v1.NewClient(cfg)
	must(err)
	defer client.Close()

	name := fmt.Sprintf("cas-%d", time.Now().Unix())
	policy := &v1.SandboxPolicy{
		Version: 1,
		Filesystem: &v1.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadOnly:       []string{"/bin", "/usr", "/lib", "/etc", "/proc", "/dev/urandom"},
			ReadWrite:      []string{"/sandbox", "/tmp", "/dev/null"},
		},
		Landlock:        &v1.LandlockPolicy{Compatibility: "best_effort"},
		NetworkPolicies: map[string]v1.NetworkPolicyRule{"base": rule("base")},
	}
	spec := &v1.SandboxSpec{Policy: policy, Command: []string{"/bin/sh", "-c", "sleep 600"}}
	if image != "" {
		spec.Template = &v1.SandboxTemplate{Image: image}
	}
	// No annotations anywhere in this program: changing annotations can advance
	// the sandbox resource version by itself and mask the defect.
	_, err = client.Sandboxes().Create(ctx, workspace, name, spec, nil)
	must(err)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := client.Sandboxes().Delete(c, workspace, name); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup failed:", err)
		}
	}()

	// Stop the sandbox so supervisor status writes cannot advance the resource
	// version between the snapshot and the policy writes.
	_, err = client.Sandboxes().WaitReady(ctx, workspace, name)
	must(err)
	_, err = client.Sandboxes().Stop(ctx, workspace, name)
	must(err)
	_, err = client.Sandboxes().WaitStopped(ctx, workspace, name)
	must(err)
	waitStable(ctx, client, name)

	// Snapshot shared by both writers.
	snap, err := client.Sandboxes().Get(ctx, workspace, name)
	must(err)
	base, err := client.Config().GetSandbox(ctx, workspace, name)
	must(err)
	R := snap.ResourceVersion
	if base.Policy == nil || R == 0 {
		fmt.Fprintln(os.Stderr, "missing baseline policy or zero resource version")
		return 1
	}
	fmt.Printf("snapshot:  resource_version=%d policy_version=%d\n", R, base.PolicyVersion)

	// Different contents for each writer (identical content could take an
	// idempotent path that creates no revision).
	a, b := clone(base.Policy), clone(base.Policy)
	a.NetworkPolicies["writer-a"] = rule("writer-a")
	b.NetworkPolicies["writer-b"] = rule("writer-b")

	// Writer A, precondition R.
	first, err := client.Config().Update(ctx, workspace, &v1.ConfigUpdate{Name: name, Policy: a, ExpectedResourceVersion: R})
	must(err)
	afterA, err := client.Sandboxes().Get(ctx, workspace, name)
	must(err)
	fmt.Printf("writer A:  accepted, policy_version=%d, sandbox resource_version=%d (was %d)\n", first.Version, afterA.ResourceVersion, R)

	// Writer B, same stale precondition R.
	second, errB := client.Config().Update(ctx, workspace, &v1.ConfigUpdate{Name: name, Policy: b, ExpectedResourceVersion: R})
	bAccepted := errB == nil
	if bAccepted {
		fmt.Printf("writer B:  ACCEPTED with stale resource_version=%d, policy_version=%d\n", R, second.Version)
	} else {
		fmt.Printf("writer B:  rejected: %v\n", errB)
	}
	bAborted := false
	if se, ok := errB.(*types.StatusError); ok {
		bAborted = codes.Code(se.GRPCCode) == codes.Aborted
		fmt.Printf("           grpc_code=%v\n", codes.Code(se.GRPCCode))
	}

	final, err := client.Config().GetSandbox(ctx, workspace, name)
	must(err)
	revs, err := client.Policy().ListAll(ctx, workspace, name)
	must(err)
	_, hasA := final.Policy.NetworkPolicies["writer-a"]
	_, hasB := final.Policy.NetworkPolicies["writer-b"]
	fmt.Printf("final:     policy_version=%d revisions=%d writer-a=%t writer-b=%t\n", final.PolicyVersion, len(revs), hasA, hasB)

	bug := bAccepted && afterA.ResourceVersion == R && !hasA && hasB
	fixed := bAborted && afterA.ResourceVersion > R && hasA && !hasB && final.PolicyVersion == first.Version
	switch {
	case bug:
		fmt.Println("RESULT: BUG REPRODUCED - stale write accepted; writer A's policy change was lost")
		return 2
	case fixed:
		// Recovery: reread version and policy, merge B into A, retry.
		cur, err := client.Sandboxes().Get(ctx, workspace, name)
		must(err)
		curPolicy, err := client.Config().GetSandbox(ctx, workspace, name)
		must(err)
		merged := clone(curPolicy.Policy)
		merged.NetworkPolicies["writer-b"] = rule("writer-b")
		_, err = client.Config().Update(ctx, workspace, &v1.ConfigUpdate{Name: name, Policy: merged, ExpectedResourceVersion: cur.ResourceVersion})
		must(err)
		out, err := client.Config().GetSandbox(ctx, workspace, name)
		must(err)
		_, hasA = out.Policy.NetworkPolicies["writer-a"]
		_, hasB = out.Policy.NetworkPolicies["writer-b"]
		fmt.Printf("retry:     accepted after reread; writer-a=%t writer-b=%t\n", hasA, hasB)
		if !hasA || !hasB {
			return 1
		}
		fmt.Println("RESULT: EXPECTED BEHAVIOR - stale write rejected with Aborted; fresh merge accepted")
		return 0
	default:
		fmt.Println("RESULT: UNEXPECTED - neither the bug signature nor the expected-fix signature; inspect output")
		return 1
	}
}

func waitStable(ctx context.Context, c *v1.Client, name string) {
	var last uint64
	for stable := 0; stable < 3; {
		s, err := c.Sandboxes().Get(ctx, workspace, name)
		must(err)
		if s.ResourceVersion == last {
			stable++
		} else {
			stable, last = 0, s.ResourceVersion
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			must(ctx.Err())
		}
	}
}

func rule(name string) v1.NetworkPolicyRule {
	return v1.NetworkPolicyRule{
		Name:      name,
		Endpoints: []v1.PolicyNetworkEndpoint{{Host: name + ".example.test", Port: 443}},
		Binaries:  []v1.PolicyNetworkBinary{{Path: "/usr/bin/curl"}},
	}
}

func clone(p *v1.SandboxPolicy) *v1.SandboxPolicy {
	b, err := json.Marshal(p)
	must(err)
	var out v1.SandboxPolicy
	must(json.Unmarshal(b, &out))
	return &out
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
