package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
)

func makeMariaDBSecret(rootPassword, wordpressPassword, exporterPassword string) ([]byte, error) {
	secret := struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]string `json:"metadata"`
		Type       string            `json:"type"`
		Data       map[string]string `json:"data"`
	}{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: map[string]string{
			"name":      "mariadb-secret",
			"namespace": "cms",
		},
		Type: "Opaque",
		Data: map[string]string{
			"MYSQL_ROOT_PASSWORD":     base64.StdEncoding.EncodeToString([]byte(rootPassword)),
			"MYSQL_WP_PASSWORD":       base64.StdEncoding.EncodeToString([]byte(wordpressPassword)),
			"MYSQL_EXPORTER_PASSWORD": base64.StdEncoding.EncodeToString([]byte(exporterPassword)),
		},
	}
	content, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("marshal Secret: %w", err)
	}
	return content, nil
}

// makeBackupSecret renders the S3 credentials Secret consumed by the backup and
// restore-drill CronJobs.
func makeBackupSecret(accessKey, secretKey string) ([]byte, error) {
	secret := struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]string `json:"metadata"`
		Type       string            `json:"type"`
		Data       map[string]string `json:"data"`
	}{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: map[string]string{
			"name":      "backup-secret",
			"namespace": "cms",
		},
		Type: "Opaque",
		Data: map[string]string{
			"S3_ACCESS_KEY": base64.StdEncoding.EncodeToString([]byte(accessKey)),
			"S3_SECRET_KEY": base64.StdEncoding.EncodeToString([]byte(secretKey)),
		},
	}
	content, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("marshal backup Secret: %w", err)
	}
	return content, nil
}

// makeBackupConfigMap renders the non-secret backup settings (endpoint, bucket,
// region, retention) consumed by the backup and restore-drill CronJobs.
func makeBackupConfigMap(endpoint, bucket, region string, retentionDays int) ([]byte, error) {
	configMap := struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]string `json:"metadata"`
		Data       map[string]string `json:"data"`
	}{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: map[string]string{
			"name":      "backup-config",
			"namespace": "cms",
		},
		Data: map[string]string{
			"S3_ENDPOINT":    endpoint,
			"S3_BUCKET":      bucket,
			"S3_REGION":      region,
			"RETENTION_DAYS": strconv.Itoa(retentionDays),
		},
	}
	content, err := json.Marshal(configMap)
	if err != nil {
		return nil, fmt.Errorf("marshal backup ConfigMap: %w", err)
	}
	return content, nil
}
