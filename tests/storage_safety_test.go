package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func storageSafetyRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func storageSafetyWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Execute the real control flow without root, mounts, Kubernetes or DRBD.
// BASH_ENV substitutes only the block-device predicate and loop delay: no
// production test switches are needed, and all mutations go to t.TempDir().
func storageSafetyDRBD(t *testing.T, script, initial, mounted, role string, loseMount bool, extraEnv ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	storageSafetyWrite(t, filepath.Join(dir, "mount-state"), initial)
	storageSafetyWrite(t, filepath.Join(dir, "kubeconfig"), "fake")
	storageSafetyWrite(t, filepath.Join(dir, "bash-env"), `function [() {
  if builtin [ "$#" -eq 3 ] && builtin [ "$1" = -b ]; then
    builtin [ "${DEVICE_PRESENT:-1}" = 1 ]; return
  fi
  builtin [ "$@"
}
sleep() {
  if [ "$LOSE_MOUNT" = 1 ] && [ ! -f "$TEST_DIR/slept" ]; then
    : > "$TEST_DIR/slept"
    : > "$TEST_DIR/mount-state"
    return
  fi
  exit 0
}
`)
	fakes := map[string]string{
		"drbdadm": `case "$1" in
  role) echo "$ROLE" ;;
  cstate) echo Connected ;;
  primary) echo "drbdadm $*" >> "$TEST_DIR/actions"; exit "${PROMOTE_EXIT:-0}" ;;
  *) echo "drbdadm $*" >> "$TEST_DIR/actions" ;;
esac`,
		"findmnt": `if [ -s "$TEST_DIR/mount-state" ]; then cat "$TEST_DIR/mount-state"; else exit 1; fi
		exit "${FINDMNT_EXIT:-0}"`,
		"mountpoint": `test -s "$TEST_DIR/mount-state"`,
		"mount": `echo "mount $*" >> "$TEST_DIR/actions"
if [ -n "$MOUNT_RESULT" ]; then printf '%s\n' "$MOUNT_RESULT" > "$TEST_DIR/mount-state"; else exit 1; fi`,
		"mkdir":  `echo "mkdir $*" >> "$TEST_DIR/actions"`,
		"umount": `echo "umount $*" >> "$TEST_DIR/actions"; : > "$TEST_DIR/mount-state"`,
		"kubectl": `echo "kubectl $*" >> "$TEST_DIR/actions"
case "$1 $2" in
  "get nodes") echo "${REGISTERED_NODE:-storage1.lab}" ;;
  "get node") echo "${PRIMARY_LABEL:-}" ;;
  "get pod") echo "${POD_NODE:-storage1.lab}" ;;
esac`,
	}
	for name, body := range fakes {
		storageSafetyWrite(t, filepath.Join(dir, name), "#!/bin/sh\n"+body+"\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{filepath.Join("..", "templates", "drbd", script)}
	if script == "drbd-failover.sh" {
		args = append(args, "promote")
	}
	cmd := exec.CommandContext(ctx, "bash", args...)
	lose := "0"
	if loseMount {
		lose = "1"
	}
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
		"BASH_ENV="+filepath.Join(dir, "bash-env"), "TEST_DIR="+dir,
		"KUBECONFIG="+filepath.Join(dir, "kubeconfig"), "HOSTNAME=storage1.lab",
		"GATEWAY=192.0.2.1", "ROLE="+role, "MOUNT_RESULT="+mounted, "LOSE_MOUNT="+lose,
		"DEVICE_PRESENT=1", "FINDMNT_EXIT=0", "REGISTERED_NODE=storage1.lab", "POD_NODE=storage1.lab", "PRIMARY_LABEL=")
	cmd.Env = append(cmd.Env, extraEnv...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("script did not terminate: %s", output)
	}
	actions, readErr := os.ReadFile(filepath.Join(dir, "actions"))
	if readErr != nil {
		t.Fatalf("no actions recorded: %v; output: %s", readErr, output)
	}
	t.Logf("%s: %s", script, output)
	return string(actions), err
}

func TestStorageSafetyDRBD(t *testing.T) {
	const valid = "/dev/drbd0 ext4 rw,relatime /\n"
	for _, script := range []string{"drbd-watchdog.sh", "drbd-failover.sh"} {
		for _, tc := range []struct {
			name, initial, mounted string
			ready, mount           bool
		}{
			{"already-mounted", valid, "", true, false},
			{"mount-success", "", valid, true, true},
			{"mount-failure", "", "", false, true},
			{"mount-succeeds-with-wrong-device", "", "/dev/vda1 ext4 rw /\n", false, true},
			{"wrong-device", "/dev/vda1 ext4 rw /\n", "", false, false},
			{"wrong-filesystem", "/dev/drbd0 xfs rw /\n", "", false, false},
			{"read-only", "/dev/drbd0 ext4 ro /\n", "", false, false},
			{"subdirectory-bind", "/dev/drbd0[/data] ext4 rw /data\n", "", false, false},
			{"ambiguous-stacked-mount", valid + "/dev/vda1 ext4 rw /\n", "", false, false},
		} {
			t.Run(script+"/"+tc.name, func(t *testing.T) {
				actions, err := storageSafetyDRBD(t, script, tc.initial, tc.mounted, "Primary", false)
				if tc.ready && err != nil || !tc.ready && script == "drbd-failover.sh" && err == nil {
					t.Fatalf("unexpected exit: %v\n%s", err, actions)
				}
				for _, publication := range []string{"drbd-status=primary", "--replicas=1"} {
					if strings.Contains(actions, publication) != tc.ready {
						t.Errorf("availability %q, ready=%v:\n%s", publication, tc.ready, actions)
					}
				}
				if !tc.ready && (!strings.Contains(actions, "drbd-status-") || !strings.Contains(actions, "--replicas=0")) {
					t.Errorf("unsafe storage must withdraw stale availability:\n%s", actions)
				}
				if strings.Contains(actions, "\nmount ") != tc.mount {
					t.Errorf("unexpected mount attempt (must not overmount), want %v:\n%s", tc.mount, actions)
				}
			})
		}
	}
}

func TestStorageSafetyWatchdogWithdrawsAfterMountLoss(t *testing.T) {
	actions, err := storageSafetyDRBD(t, "drbd-watchdog.sh", "/dev/drbd0 ext4 rw /\n", "", "Primary", true)
	if err != nil {
		t.Fatal(err)
	}
	published := strings.Index(actions, "--replicas=1")
	withdrawn := strings.Index(actions, "--replicas=0")
	if published < 0 || withdrawn < published || strings.Contains(actions[withdrawn:], "--replicas=1") {
		t.Fatalf("mount loss must withdraw availability and not re-enable DB:\n%s", actions)
	}
}

func TestStorageSafetyUnknownRoleWithdrawsLocalAvailability(t *testing.T) {
	actions, err := storageSafetyDRBD(t, "drbd-watchdog.sh", "", "", "Unknown", false)
	if err != nil || !strings.Contains(actions, "drbd-status-") || !strings.Contains(actions, "--replicas=0") || strings.Contains(actions, "--replicas=1") {
		t.Fatalf("unknown role must fail closed: %v\n%s", err, actions)
	}
}

func TestStorageSafetyDRBDVerificationFailures(t *testing.T) {
	for _, script := range []string{"drbd-watchdog.sh", "drbd-failover.sh"} {
		for _, env := range []string{"DEVICE_PRESENT=0", "FINDMNT_EXIT=1", "REGISTERED_NODE=other.lab"} {
			t.Run(script+"/"+env, func(t *testing.T) {
				initial := "/dev/drbd0 ext4 rw /\n"
				if strings.HasPrefix(env, "REGISTERED_NODE=") {
					initial = "" // Even without a resolved node, a Primary must stop DB.
				}
				actions, _ := storageSafetyDRBD(t, script, initial, "", "Primary", false, env)
				if strings.Contains(actions, "--replicas=1") || strings.Contains(actions, "drbd-status=primary") || !strings.Contains(actions, "--replicas=0") {
					t.Fatalf("verification failure must withdraw availability:\n%s", actions)
				}
			})
		}
	}
}

func TestStorageSafetyUnknownNodeDoesNotStopHealthyPeer(t *testing.T) {
	actions, err := storageSafetyDRBD(t, "drbd-watchdog.sh", "", "", "Unknown", false, "POD_NODE=storage2.lab")
	if err != nil || strings.Contains(actions, "--replicas=") {
		t.Fatalf("passive node must not scale a healthy peer DB: %v\n%s", err, actions)
	}
}

func TestStorageSafetyRejectedPromotionDoesNotStopHealthyPeer(t *testing.T) {
	actions, err := storageSafetyDRBD(t, "drbd-failover.sh", "", "", "Secondary", false, "PROMOTE_EXIT=1", "POD_NODE=storage2.lab")
	if err == nil || strings.Contains(actions, "--replicas=") {
		t.Fatalf("rejected promotion must not scale a healthy peer DB: %v\n%s", err, actions)
	}
}

func TestStorageSafetyUnverifiedStorageDoesNotDeletePeerPod(t *testing.T) {
	for _, script := range []string{"drbd-watchdog.sh", "drbd-failover.sh"} {
		actions, _ := storageSafetyDRBD(t, script, "/dev/vda1 ext4 rw /\n", "", "Primary", false, "POD_NODE=storage2.lab")
		if strings.Contains(actions, "delete pod") || strings.Contains(actions, "--replicas=1") {
			t.Fatalf("unverified storage must not reschedule DB:\n%s", actions)
		}
	}
}

func TestStorageSafetyMariaDB(t *testing.T) {
	config := storageSafetyRead(t, "kubernetes/base/mariadb-configmap.yaml")
	if !strings.Contains(config, "innodb_flush_log_at_trx_commit=1") {
		t.Fatal("InnoDB must flush the redo log on every commit")
	}
	pv := storageSafetyRead(t, "kubernetes/base/mariadb-pv.yaml")
	if !strings.Contains(pv, "type: Directory\n") || strings.Contains(pv, "DirectoryOrCreate") {
		t.Fatal("MariaDB must not create a local data directory as fallback")
	}
}

func TestStorageSafetyUploadsGuard(t *testing.T) {
	manifest := storageSafetyRead(t, "puppet/modules/role/manifests/cms_frontend.pp")
	start := strings.Index(manifest, "content => @(UPLOADSCHECK)\n")
	if start < 0 {
		t.Fatal("missing uploads guard")
	}
	body := strings.SplitN(manifest[start:], "\n", 2)[1]
	body = strings.SplitN(body, "      | UPLOADSCHECK", 2)[0]
	body = strings.ReplaceAll(body, "      ", "")
	// Only redirect findmnt to a fake. The awk predicate is executed unchanged.
	body = strings.ReplaceAll(body, "/usr/bin/findmnt", "findmnt")
	dir := t.TempDir()
	guard := filepath.Join(dir, "check-uploads")
	storageSafetyWrite(t, guard, body)
	storageSafetyWrite(t, filepath.Join(dir, "findmnt"), "#!/bin/sh\nprintf '%s\\n' \"$MOUNT_INFO\"\nexit \"${FINDMNT_EXIT:-0}\"\n")
	for _, tc := range []struct {
		name, info string
		ok         bool
	}{
		{"nfs4", "192.0.2.15:/srv/nfs/wp-uploads nfs4 rw,hard,vers=4.2 /", true},
		{"nfs", "192.0.2.15:/srv/nfs/wp-uploads nfs rw,hard,vers=4 /", true},
		{"absent", "", false},
		{"local", "/dev/vda1 ext4 rw /", false},
		{"wrong-host", "192.0.2.16:/srv/nfs/wp-uploads nfs4 rw,hard /", false},
		{"wrong-export", "192.0.2.15:/other nfs4 rw,hard /", false},
		{"readonly", "192.0.2.15:/srv/nfs/wp-uploads nfs4 ro,hard /", false},
		{"soft", "192.0.2.15:/srv/nfs/wp-uploads nfs4 rw,soft /", false},
		{"softreval", "192.0.2.15:/srv/nfs/wp-uploads nfs4 rw,hard,softreval /", false},
		{"bind", "192.0.2.15:/srv/nfs/wp-uploads nfs4 rw,hard /subdir", false},
		{"ambiguous-stacked-mount", "192.0.2.15:/srv/nfs/wp-uploads nfs4 rw,hard /\n/dev/vda1 ext4 rw /", false},
		{"findmnt-failure", "192.0.2.15:/srv/nfs/wp-uploads nfs4 rw,hard /", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", guard, "192.0.2.15:/srv/nfs/wp-uploads")
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "MOUNT_INFO="+tc.info)
			if tc.name == "findmnt-failure" {
				cmd.Env = append(cmd.Env, "FINDMNT_EXIT=1")
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v, error=%v: %s", tc.ok, err, output)
			}
		})
	}
}

func storageSafetyUploadsHeredoc(t *testing.T, manifest, tag string) string {
	t.Helper()
	marker := "content => @(" + tag + ")\n"
	start := strings.Index(manifest, marker)
	if start < 0 {
		t.Fatalf("missing uploads script %s", tag)
	}
	body := strings.SplitN(manifest[start:], "\n", 2)[1]
	body = strings.SplitN(body, "      | "+tag, 2)[0]
	return strings.ReplaceAll(body, "      ", "")
}

func TestStorageSafetyUploadsMigration(t *testing.T) {
	const export = "192.0.2.15:/srv/nfs/wp-uploads"
	const hard = export + " nfs4 rw,hard,vers=4.2 /\n"
	const soft = export + " nfs4 rw,soft,timeo=50,retrans=3 /\n"
	manifest := storageSafetyRead(t, "puppet/modules/role/manifests/cms_frontend.pp")
	// Exercise the actual Puppet exec, including the old implementation when
	// reproducing the regression. Every storage/systemd operation is replaced
	// by an absolute temporary fake; no real mount command can be executed.
	block := strings.SplitN(strings.SplitN(manifest, "exec { 'mount-wp-uploads':", 2)[1], "\n  }", 2)[0]
	var command string
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "command => ") {
			command = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "command => \""), "\",")
		}
	}
	if command == "" {
		t.Fatal("missing mount-wp-uploads command")
	}
	for _, tc := range []struct {
		name, initial               string
		env                         []string
		ok, remount, unmount, mount bool
	}{
		{name: "already-hard", initial: hard, ok: true},
		{name: "legacy-soft-remount", initial: soft, ok: true, remount: true},
		{name: "legacy-nfs-remount", initial: strings.Replace(soft, "nfs4", "nfs", 1), ok: true, remount: true},
		{name: "unsupported-remount", initial: soft, env: []string{"REMOUNT_EXIT=32"}, ok: true, remount: true, unmount: true, mount: true},
		{name: "ineffective-remount", initial: soft, env: []string{"REMOUNT_RESULT=" + soft}, ok: true, remount: true, unmount: true, mount: true},
		{name: "busy-unmount", initial: soft, env: []string{"REMOUNT_EXIT=32", "UMOUNT_EXIT=1"}, remount: true, unmount: true},
		{name: "replacement-mount-fails", initial: soft, env: []string{"REMOUNT_EXIT=32", "MOUNT_EXIT=1"}, remount: true, unmount: true, mount: true},
		{name: "replacement-still-soft", initial: soft, env: []string{"REMOUNT_EXIT=32", "MOUNT_RESULT=" + soft}, remount: true, unmount: true, mount: true},
		{name: "replacement-wrong-export", initial: soft, env: []string{"REMOUNT_EXIT=32", "MOUNT_RESULT=192.0.2.16:/other nfs4 rw,hard /\n"}, remount: true, unmount: true, mount: true},
		{name: "identity-changes-during-remount", initial: soft, env: []string{"REMOUNT_RESULT=192.0.2.16:/other nfs4 rw,soft /\n"}, remount: true},
		{name: "unexpected-underlying-mount", initial: soft, env: []string{"REMOUNT_EXIT=32", "UMOUNT_RESULT=/dev/vda1 ext4 rw /\n"}, remount: true, unmount: true},
		{name: "absent", ok: true, mount: true},
		{name: "stop-fails", initial: soft, env: []string{"STOP_EXIT=1"}},
		{name: "apache-still-active", initial: soft, env: []string{"STOP_STATE=active"}},
		{name: "cannot-confirm-apache-stopped", initial: soft, env: []string{"SHOW_EXIT=1"}},
		{name: "findmnt-fails", initial: soft, env: []string{"FINDMNT_EXIT=1"}},
		{name: "wrong-server", initial: "192.0.2.16:/srv/nfs/wp-uploads nfs4 rw,soft /\n"},
		{name: "wrong-export", initial: "192.0.2.15:/other nfs4 rw,soft /\n"},
		{name: "wrong-filesystem", initial: export + " ext4 rw,soft /\n"},
		{name: "read-only", initial: export + " nfs4 ro,soft /\n"},
		{name: "conflicting-write-mode", initial: export + " nfs4 rw,ro,soft /\n"},
		{name: "subdirectory-bind", initial: export + " nfs4 rw,soft /subdir\n"},
		{name: "stacked-mounts", initial: soft + "/dev/vda1 ext4 rw /\n"},
		{name: "unknown-options", initial: export + " nfs4 rw /\n"},
		{name: "softreval", initial: export + " nfs4 rw,hard,softreval /\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			storageSafetyWrite(t, filepath.Join(dir, "mount-state"), tc.initial)
			storageSafetyWrite(t, filepath.Join(dir, "apache-state"), "active\n")
			storageSafetyWrite(t, filepath.Join(dir, "actions"), "")
			rewrite := strings.NewReplacer(
				"/usr/local/sbin/cms-ha-check-uploads", filepath.Join(dir, "check-uploads"),
				"/usr/local/sbin/cms-ha-mount-uploads", filepath.Join(dir, "migrate-uploads"),
				"/usr/bin/findmnt", filepath.Join(dir, "findmnt"),
				"/bin/mountpoint", filepath.Join(dir, "mountpoint"),
				"/bin/mount", filepath.Join(dir, "mount"),
				"/bin/umount", filepath.Join(dir, "umount"),
				"/bin/systemctl", filepath.Join(dir, "systemctl"),
				"/bin/chown", filepath.Join(dir, "chown"),
				"/bin/chmod", filepath.Join(dir, "chmod"),
				"${storage_ip}", "192.0.2.15",
			)
			for tag, name := range map[string]string{"UPLOADSCHECK": "check-uploads", "UPLOADSMOUNT": "migrate-uploads"} {
				if tag == "UPLOADSMOUNT" && !strings.Contains(manifest, "content => @("+tag+")") {
					continue // The pre-fix exec has no migration helper.
				}
				body := rewrite.Replace(storageSafetyUploadsHeredoc(t, manifest, tag))
				for _, forbidden := range []string{"/bin/mount", "/bin/umount", "/bin/systemctl", "/bin/chown", "/bin/chmod"} {
					if strings.Contains(body, forbidden) {
						t.Fatalf("unfaked command %s", forbidden)
					}
				}
				storageSafetyWrite(t, filepath.Join(dir, name), body)
			}
			fakes := map[string]string{
				"findmnt": `cat "$TEST_DIR/mount-state"
[ -s "$TEST_DIR/mount-state" ] && exit "$FINDMNT_EXIT"
exit 1`,
				"mountpoint": `test -s "$TEST_DIR/mount-state"`,
				"systemctl": `echo "systemctl $*" >> "$TEST_DIR/actions"
case "$1" in
  stop) [ "$STOP_EXIT" = 0 ] || exit "$STOP_EXIT"; printf '%s\n' "$STOP_STATE" > "$TEST_DIR/apache-state" ;;
  show) [ "$SHOW_EXIT" = 0 ] || exit "$SHOW_EXIT"; cat "$TEST_DIR/apache-state" ;;
  start) echo active > "$TEST_DIR/apache-state" ;;
  *) exit 97 ;;
esac`,
				"mount": `echo "mount $*" >> "$TEST_DIR/actions"
[ "$(cat "$TEST_DIR/apache-state")" = inactive ] || exit 97
case "$*" in
  *remount,*) [ "$REMOUNT_EXIT" = 0 ] || exit "$REMOUNT_EXIT"; printf '%s' "$REMOUNT_RESULT" > "$TEST_DIR/mount-state" ;;
  *) [ ! -s "$TEST_DIR/mount-state" ] || exit 97; [ "$MOUNT_EXIT" = 0 ] || exit "$MOUNT_EXIT"; printf '%s' "$MOUNT_RESULT" > "$TEST_DIR/mount-state" ;;
esac`,
				"umount": `echo "umount $*" >> "$TEST_DIR/actions"
[ "$(cat "$TEST_DIR/apache-state")" = inactive ] && [ "$#" = 1 ] && [ "$1" = /var/www/html/wp-content/uploads ] || exit 97
[ "$UMOUNT_EXIT" = 0 ] || exit "$UMOUNT_EXIT"
printf '%s' "$UMOUNT_RESULT" > "$TEST_DIR/mount-state"`,
				"chown": `echo "chown $*" >> "$TEST_DIR/actions"
[ ! -s "$TEST_DIR/mount-state" ] && [ "$(cat "$TEST_DIR/apache-state")" = inactive ]`,
				"chmod": `echo "chmod $*" >> "$TEST_DIR/actions"
[ ! -s "$TEST_DIR/mount-state" ] && [ "$(cat "$TEST_DIR/apache-state")" = inactive ]`,
			}
			for name, body := range fakes {
				storageSafetyWrite(t, filepath.Join(dir, name), "#!/bin/sh\nset -eu\n"+body+"\n")
			}
			env := append(os.Environ(), "TEST_DIR="+dir, "REMOUNT_EXIT=0", "REMOUNT_RESULT="+hard,
				"MOUNT_EXIT=0", "MOUNT_RESULT="+hard, "UMOUNT_EXIT=0", "UMOUNT_RESULT=", "FINDMNT_EXIT=0",
				"STOP_EXIT=0", "STOP_STATE=inactive", "SHOW_EXIT=0")
			env = append(env, tc.env...)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// Model Puppet's stop-before-mount and start-only-after-success edges.
			check := filepath.Join(dir, "check-uploads") + " " + export
			control := filepath.Join(dir, "systemctl")
			run := "if ! " + check + "; then " + control + " stop apache2 || exit 1; fi; " + rewrite.Replace(command) + " && " + check + " && " + control + " start apache2"
			cmd := exec.CommandContext(ctx, "sh", "-c", run)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			actions := storageSafetyReadAbsolute(t, filepath.Join(dir, "actions"))
			if (err == nil) != tc.ok {
				t.Fatalf("success=%v, want %v: %v\n%s\n%s", err == nil, tc.ok, err, out, actions)
			}
			for _, want := range []struct {
				needle string
				yes    bool
			}{
				{"mount -o remount,", tc.remount}, {"umount ", tc.unmount}, {"mount -t nfs ", tc.mount},
				{"systemctl start apache2", tc.ok},
			} {
				if strings.Contains(actions, want.needle) != want.yes {
					t.Errorf("unexpected action %q, want %v:\n%s", want.needle, want.yes, actions)
				}
			}
			if tc.remount || tc.mount {
				stop := strings.Index(actions, "systemctl stop apache2")
				change := strings.Index(actions, "\nmount ")
				if stop < 0 || change < stop {
					t.Errorf("Apache must stop before storage changes:\n%s", actions)
				}
			}
			if tc.remount && !strings.Contains(actions, "mount -o remount,rw,hard,timeo=600,retrans=2,nfsvers=4 "+export+" /var/www/html/wp-content/uploads") {
				t.Errorf("remount must use the explicit expected source and safe options:\n%s", actions)
			}
			if tc.mount && !strings.Contains(actions, "mount -t nfs -o rw,hard,timeo=600,retrans=2,nfsvers=4 "+export+" /var/www/html/wp-content/uploads") {
				t.Errorf("fresh mount must use the explicit expected source and safe options:\n%s", actions)
			}
			if !tc.ok && tc.name != "stop-fails" && tc.name != "apache-still-active" && storageSafetyReadAbsolute(t, filepath.Join(dir, "apache-state")) != "inactive\n" {
				t.Errorf("migration failure must leave Apache stopped:\n%s", actions)
			}
			if tc.mount && tc.ok && !strings.Contains(actions, "chmod 0555 /var/www/html/wp-content/uploads") {
				t.Errorf("exposed local directory must be protected before mounting:\n%s", actions)
			}
			if tc.ok {
				// A converged node must not be stopped or remounted on the next run.
				cmd = exec.CommandContext(ctx, "sh", "-c", rewrite.Replace(command))
				cmd.Env = env
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("idempotent rerun failed: %v\n%s", err, out)
				}
				if got := storageSafetyReadAbsolute(t, filepath.Join(dir, "actions")); got != actions {
					t.Errorf("converged rerun changed storage or Apache:\n%s", got)
				}
			}
		})
	}
}

func storageSafetyReadAbsolute(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStorageSafetyPuppetOrdering(t *testing.T) {
	if _, err := exec.LookPath("ruby"); err != nil {
		t.Skip("Ruby is not installed")
	}
	if out, err := exec.Command("ruby", "-rpuppet", "-e", "puts Puppet.version").CombinedOutput(); err != nil {
		t.Skipf("Puppet Ruby library is not available: %s", out)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "catalog.rb")
	storageSafetyWrite(t, script, `require 'puppet'
root, cache = ARGV
Puppet.initialize_settings([
  '--confdir', File.join(cache, 'conf'), '--vardir', File.join(cache, 'var'),
  '--logdir', File.join(cache, 'log'), '--rundir', File.join(cache, 'run'),
  '--modulepath', File.join(root, 'puppet/modules'),
  '--hiera_config', File.join(root, 'puppet/hiera.yaml'),
  '--code', 'include role::cms_frontend'
])
# mount_core is not bundled with the local Puppet gem. If unavailable, stub
# only its resource schema; the catalog, core autorequires and relationships
# are real. No provider is applied and no machine configuration is changed.
unless Puppet::Type.type(:mount)
  Puppet::Type.newtype(:mount) do
    newparam(:name, namevar: true)
    ensurable do
      newvalue(:present)
      newvalue(:absent)
      newvalue(:mounted)
      newvalue(:unmounted)
    end
    [:device, :fstype, :options, :dump, :pass, :atboot, :target, :remounts, :blockdevice].each { |p| newparam(p) }
  end
  puts 'mount_core absent: checking graph with mount schema stub'
end
begin
  node = Puppet::Node.new('storage-safety', facts: Puppet::Node::Facts.new('storage-safety', {
    'networking' => {'fqdn' => 'storage-safety.test', 'hostname' => 'storage-safety'},
    'os' => {'family' => 'Debian', 'name' => 'Ubuntu', 'release' => {'major' => '24.04'}}
  }))
  catalog = Puppet::Parser::Compiler.compile(node)
  # Graph construction validates exec users even though no exec is run.
  # Omit only the root execution identity so this check can run unprivileged;
  # root is not a managed User resource and contributes no graph edge here.
  catalog.resources.each do |resource|
    resource.parameters.delete(:user) if resource.type == 'Exec' && resource[:user] == 'root'
  end
  graph = catalog.to_ral.relationship_graph
  cycles = graph.find_cycles_in_graph
  raise "Dependency cycles: #{cycles.map { |cycle| cycle.map(&:to_s).join(' -> ') }.join('; ')}" unless cycles.empty?
  puts 'Frontend catalog dependency graph is acyclic'
rescue => error
  warn error.message
  exit 1
end
`)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ruby", script, root, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("Puppet catalog ordering: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func TestStorageSafetyFrontendWiring(t *testing.T) {
	manifest := storageSafetyRead(t, "puppet/modules/role/manifests/cms_frontend.pp")
	for _, want := range []string{
		"php_admin_value memory_limit 128M", "ServerLimit 4", "MaxRequestWorkers 4",
		"RequiresMountsFor=/var/www/html/wp-content/uploads", "BindsTo=${uploads_mount_unit}", "After=${uploads_mount_unit}",
		`$uploads_mount_unit = 'var-www-html-wp\x2dcontent-uploads.mount'`,
		"ExecStartPre=/usr/local/sbin/cms-ha-check-uploads ${storage_ip}:/srv/nfs/wp-uploads",
		"_netdev,auto,rw,hard,timeo=600,retrans=2,nfsvers=4", "Exec['apache-storage-daemon-reload']",
		"exec { 'apache-stop-unsafe-uploads':", "command => '/bin/systemctl stop apache2'",
		"before  => Exec['mount-wp-uploads']",
		"/bin/chown root:root /var/www/html/wp-content/uploads && /bin/chmod 0555",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("missing storage/resource safety wiring: %s", want)
		}
	}
	for _, unsafe := range []string{"memory_limit -1", "nofail", "rw,soft,", "/dev/tcp/"} {
		if strings.Contains(manifest, unsafe) {
			t.Errorf("unsafe frontend setting: %s", unsafe)
		}
	}
	block := func(header string) string {
		t.Helper()
		parts := strings.SplitN(manifest, header, 2)
		if len(parts) != 2 {
			t.Fatalf("missing resource %s", header)
		}
		return strings.SplitN(parts[1], "\n  }", 2)[0]
	}
	mountExec := block("exec { 'mount-wp-uploads':")
	if !strings.Contains(mountExec, "File['/usr/local/sbin/cms-ha-mount-uploads']") ||
		!strings.Contains(mountExec, "Exec['apache-stop-unsafe-uploads']") ||
		strings.Contains(mountExec, "Mount['/var/www/html/wp-content/uploads']") {
		t.Error("verified live migration must run after stopping Apache but before the fstab provider")
	}
	fstab := block("mount { '/var/www/html/wp-content/uploads':")
	if !strings.Contains(fstab, "Exec['mount-wp-uploads']") {
		t.Error("fstab updates must be blocked by a failed or unexpected live mount")
	}
	reload := block("exec { 'apache-storage-daemon-reload':")
	if !strings.Contains(reload, "subscribe   => Mount['/var/www/html/wp-content/uploads']") ||
		!strings.Contains(reload, "before      => Service['apache2']") {
		t.Error("daemon reload must follow fstab updates and precede Apache startup")
	}
	// A failed verified mount must block both Apache and the shared install lock.
	for _, resource := range []string{"service { 'apache2':", "exec { 'wordpress-core-install':"} {
		block := strings.SplitN(strings.SplitN(manifest, resource, 2)[1], "\n  }", 2)[0]
		if !strings.Contains(block, "Exec['mount-wp-uploads']") {
			t.Errorf("%s must depend on verified NFS mount", resource)
		}
	}
}
