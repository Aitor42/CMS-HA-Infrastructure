package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMariaDBConfigMapIsActuallyMounted keeps the InnoDB safety settings in
// the manifest, not only in the operations manual. Without the mount MariaDB
// falls back to native AIO on the DRBD+qcow2 device, which is the failure mode
// the manual documents.
func TestMariaDBConfigMapIsActuallyMounted(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}

	configMap := read("kubernetes/base/mariadb-configmap.yaml")
	for _, want := range []string{"name: mariadb-config", "innodb_use_native_aio=0", "innodb_flush_method=fsync"} {
		if !strings.Contains(configMap, want) {
			t.Errorf("mariadb-configmap.yaml missing %q", want)
		}
	}

	kustomization := read("kubernetes/base/kustomization.yaml")
	if !strings.Contains(kustomization, "mariadb-configmap.yaml") {
		t.Error("kubernetes/base/kustomization.yaml does not include the MariaDB ConfigMap")
	}

	statefulSet := read("kubernetes/base/mariadb-statefulset.yaml")
	for _, want := range []string{
		"name: mariadb-config",
		"mountPath: /etc/mysql/conf.d/custom.cnf",
		"subPath: custom.cnf",
		"configMap:",
	} {
		if !strings.Contains(statefulSet, want) {
			t.Errorf("mariadb-statefulset.yaml missing %q", want)
		}
	}
}
