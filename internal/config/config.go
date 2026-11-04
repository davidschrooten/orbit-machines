// Package config provides typed, validated 12-factor configuration loading
// for orbit-dispatcher and orbit-machined using spf13/viper and spf13/pflag.
package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/ticket"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// DispatcherConfig holds all runtime configuration for cmd/orbit-dispatcher.
type DispatcherConfig struct {
	ListenAddr          string
	OrbitURL            string
	ClusterToken        string
	IngressHost         string
	KubernetesNamespace string
	MachineImage        string
	MaxConcurrentPods   int
	HeartbeatInterval   time.Duration
	S3Endpoint          string
	S3AccessKey         string
	S3SecretKey         string
	S3ProfileBucket     string
	S3NixCacheBucket    string
}

// Validate checks invariants on DispatcherConfig.
func (c DispatcherConfig) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return fmt.Errorf("%w: listen_addr is required", domain.ErrInvalidConfig)
	}
	parsedURL, err := url.Parse(strings.TrimSpace(c.OrbitURL))
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return fmt.Errorf("%w: orbit_url must be a valid http or https URL", domain.ErrInvalidConfig)
	}
	if len(strings.TrimSpace(c.ClusterToken)) < ticket.MinSecretLength {
		return fmt.Errorf("%w: cluster_token must be at least %d characters", domain.ErrInvalidConfig, ticket.MinSecretLength)
	}
	if strings.TrimSpace(c.IngressHost) == "" {
		return fmt.Errorf("%w: ingress_host is required", domain.ErrInvalidConfig)
	}
	if c.MaxConcurrentPods <= 0 {
		return fmt.Errorf("%w: max_concurrent_pods must be > 0", domain.ErrInvalidConfig)
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("%w: heartbeat_interval must be > 0", domain.ErrInvalidConfig)
	}
	return nil
}

// BindDispatcherFlags registers CLI flags on flags and binds them to v alongside
// ORBIT_DISPATCHER_* environment variables and default values.
func BindDispatcherFlags(v *viper.Viper, flags *pflag.FlagSet) error {
	v.SetEnvPrefix("ORBIT_DISPATCHER")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()

	flags.String("listen-addr", ":8080", "HTTP/WebSocket listen address")
	flags.String("orbit-url", "", "Base URL of the Orbit Rails control plane")
	flags.String("cluster-token", "", "Shared cluster bearer token (omc_live_...)")
	flags.String("ingress-host", "", "Public WebSocket ingress hostname for this cluster")
	flags.String("kubernetes-namespace", "orbit-machines", "Kubernetes namespace for Machine Pods")
	flags.String("machine-image", "ghcr.io/davidschrooten/orbit-machined:latest", "Container image for Machine Pods")
	flags.Int("max-concurrent-pods", 25, "Maximum concurrent Machine Pods in this cluster")
	flags.Duration("heartbeat-interval", 30*time.Second, "Control-plane heartbeat interval")
	flags.String("s3-endpoint", "http://orbit-machines-minio:9000", "S3/MinIO endpoint URL")
	flags.String("s3-access-key", "", "S3/MinIO access key ID")
	flags.String("s3-secret-key", "", "S3/MinIO secret access key")
	flags.String("s3-profile-bucket", "orbit-user-profiles", "Bucket for encrypted user profiles and WIP git bundles")
	flags.String("s3-nix-cache-bucket", "orbit-nix-cache", "Bucket for shared Nix binary cache")

	bindings := map[string]string{
		"listen_addr":          "listen-addr",
		"orbit_url":            "orbit-url",
		"cluster_token":        "cluster-token",
		"ingress_host":         "ingress-host",
		"kubernetes_namespace": "kubernetes-namespace",
		"machine_image":        "machine-image",
		"max_concurrent_pods":  "max-concurrent-pods",
		"heartbeat_interval":   "heartbeat-interval",
		"s3_endpoint":          "s3-endpoint",
		"s3_access_key":        "s3-access-key",
		"s3_secret_key":        "s3-secret-key",
		"s3_profile_bucket":    "s3-profile-bucket",
		"s3_nix_cache_bucket":  "s3-nix-cache-bucket",
	}
	for key, flagName := range bindings {
		_ = v.BindPFlag(key, flags.Lookup(flagName))
	}
	return nil
}

// LoadDispatcher reads an optional configFile into v and returns a validated DispatcherConfig.
func LoadDispatcher(v *viper.Viper, configFile string) (DispatcherConfig, error) {
	if strings.TrimSpace(configFile) != "" {
		v.SetConfigFile(configFile)
		if err := v.ReadInConfig(); err != nil {
			return DispatcherConfig{}, fmt.Errorf("%w: reading config file %q: %w", domain.ErrInvalidConfig, configFile, err)
		}
	}

	cfg := DispatcherConfig{
		ListenAddr:          v.GetString("listen_addr"),
		OrbitURL:            v.GetString("orbit_url"),
		ClusterToken:        v.GetString("cluster_token"),
		IngressHost:         v.GetString("ingress_host"),
		KubernetesNamespace: v.GetString("kubernetes_namespace"),
		MachineImage:        v.GetString("machine_image"),
		MaxConcurrentPods:   v.GetInt("max_concurrent_pods"),
		HeartbeatInterval:   v.GetDuration("heartbeat_interval"),
		S3Endpoint:          v.GetString("s3_endpoint"),
		S3AccessKey:         v.GetString("s3_access_key"),
		S3SecretKey:         v.GetString("s3_secret_key"),
		S3ProfileBucket:     v.GetString("s3_profile_bucket"),
		S3NixCacheBucket:    v.GetString("s3_nix_cache_bucket"),
	}
	if err := cfg.Validate(); err != nil {
		return DispatcherConfig{}, err
	}
	return cfg, nil
}

// MachinedConfig holds all runtime configuration for cmd/orbit-machined inside a Machine Pod.
type MachinedConfig struct {
	ListenAddr              string
	MachineID               domain.MachineID
	ResourceTier            domain.ResourceTier
	WorkDir                 string
	ProfileEncryptionKeyHex string
	ProfileEncryptionKey    []byte
	S3Endpoint              string
	S3ProfileBucket         string
	S3NixCacheBucket        string
	IdleTimeout             time.Duration
}

// BindMachinedFlags registers CLI flags on flags and binds them to v alongside
// ORBIT_MACHINED_* environment variables and default values.
func BindMachinedFlags(v *viper.Viper, flags *pflag.FlagSet) error {
	v.SetEnvPrefix("ORBIT_MACHINED")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()

	flags.String("listen-addr", ":8080", "In-Pod WebSocket compositor listen address")
	flags.String("workspace-id", "", "Orbit workspace identifier")
	flags.String("user-id", "", "Orbit user identifier")
	flags.String("resource-tier", string(domain.TierStandard), "Pod compute resource tier (standard or performance)")
	flags.String("work-dir", "/home/orbit/workspace", "Root directory for cloned workspace repositories")
	flags.String("profile-encryption-key", "", "Hex-encoded 32-byte AES-256-GCM key for user profile archives")
	flags.String("s3-endpoint", "", "S3/MinIO endpoint URL")
	flags.String("s3-profile-bucket", "orbit-user-profiles", "Bucket for encrypted user profiles and WIP git bundles")
	flags.String("s3-nix-cache-bucket", "orbit-nix-cache", "Bucket for shared Nix binary cache")
	flags.Duration("idle-timeout", 30*time.Minute, "Inactivity duration before snapshotting WIP and shutting down")

	bindings := map[string]string{
		"listen_addr":            "listen-addr",
		"workspace_id":           "workspace-id",
		"user_id":                "user-id",
		"resource_tier":          "resource-tier",
		"work_dir":               "work-dir",
		"profile_encryption_key": "profile-encryption-key",
		"s3_endpoint":            "s3-endpoint",
		"s3_profile_bucket":      "s3-profile-bucket",
		"s3_nix_cache_bucket":    "s3-nix-cache-bucket",
		"idle_timeout":           "idle-timeout",
	}
	for key, flagName := range bindings {
		_ = v.BindPFlag(key, flags.Lookup(flagName))
	}
	return nil
}

// LoadMachined reads an optional configFile into v and returns a validated MachinedConfig.
func LoadMachined(v *viper.Viper, configFile string) (MachinedConfig, error) {
	if strings.TrimSpace(configFile) != "" {
		v.SetConfigFile(configFile)
		if err := v.ReadInConfig(); err != nil {
			return MachinedConfig{}, fmt.Errorf("%w: reading config file %q: %w", domain.ErrInvalidConfig, configFile, err)
		}
	}

	listenAddr := strings.TrimSpace(v.GetString("listen_addr"))
	if listenAddr == "" {
		return MachinedConfig{}, fmt.Errorf("%w: listen_addr is required", domain.ErrInvalidConfig)
	}

	mid, err := domain.NewMachineID(v.GetString("workspace_id"), v.GetString("user_id"))
	if err != nil {
		return MachinedConfig{}, fmt.Errorf("%w: %w", domain.ErrInvalidConfig, err)
	}

	tier, err := domain.ParseResourceTier(v.GetString("resource_tier"))
	if err != nil {
		return MachinedConfig{}, fmt.Errorf("%w: %w", domain.ErrInvalidConfig, err)
	}

	idleTimeout := v.GetDuration("idle_timeout")
	if idleTimeout <= 0 {
		return MachinedConfig{}, fmt.Errorf("%w: idle_timeout must be > 0", domain.ErrInvalidConfig)
	}

	s3Endpoint := strings.TrimSpace(v.GetString("s3_endpoint"))
	keyHex := strings.TrimSpace(v.GetString("profile_encryption_key"))
	var keyBytes []byte
	if keyHex != "" {
		decoded, decErr := hex.DecodeString(keyHex)
		if decErr != nil {
			return MachinedConfig{}, fmt.Errorf("%w: profile_encryption_key must be valid hex: %w", domain.ErrInvalidConfig, decErr)
		}
		keyBytes = decoded
	}
	if (s3Endpoint != "" || keyHex != "") && len(keyBytes) != 32 {
		return MachinedConfig{}, fmt.Errorf("%w: profile_encryption_key must be 32 bytes (64 hex chars), got %d bytes", domain.ErrInvalidConfig, len(keyBytes))
	}

	return MachinedConfig{
		ListenAddr:              listenAddr,
		MachineID:               mid,
		ResourceTier:            tier,
		WorkDir:                 v.GetString("work_dir"),
		ProfileEncryptionKeyHex: keyHex,
		ProfileEncryptionKey:    keyBytes,
		S3Endpoint:              s3Endpoint,
		S3ProfileBucket:         v.GetString("s3_profile_bucket"),
		S3NixCacheBucket:        v.GetString("s3_nix_cache_bucket"),
		IdleTimeout:             idleTimeout,
	}, nil
}
