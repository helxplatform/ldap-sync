package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// pvcGroupConfig is the YAML-decodable shape of the pvc-group plugin's
// options block. It mirrors the keys under PluginConfig.Options.
type pvcGroupConfig struct {
	Namespace      string   `yaml:"namespace"`
	StorageClass   string   `yaml:"storageClass"`
	AccessModes    []string `yaml:"accessModes"`
	Size           string   `yaml:"size"`
	GroupLabelKey  string   `yaml:"groupLabelKey"`
	ExtraLabels    map[string]string `yaml:"extraLabels"`
	NamePrefix     string   `yaml:"namePrefix"`
	MatchObjectClass string `yaml:"matchObjectClass"`
	Kubeconfig     string   `yaml:"kubeconfig"`
}

const (
	defaultGroupLabelKey  = "helx.renci.org/group-name"
	defaultMatchObjectClass = "groupOfNames"
	defaultNamePrefix     = "group-"
	defaultSize           = "1Gi"
)

func (c pvcGroupConfig) withDefaults() pvcGroupConfig {
	if c.GroupLabelKey == "" {
		c.GroupLabelKey = defaultGroupLabelKey
	}
	if c.MatchObjectClass == "" {
		c.MatchObjectClass = defaultMatchObjectClass
	}
	if c.NamePrefix == "" {
		c.NamePrefix = defaultNamePrefix
	}
	if c.Size == "" {
		c.Size = defaultSize
	}
	if len(c.AccessModes) == 0 {
		c.AccessModes = []string{"ReadWriteMany"}
	}
	return c
}

// pvcClient is the slice of kubernetes.Interface the plugin actually uses.
// Tests substitute a fake client implementing this surface.
type pvcClient interface {
	List(ctx context.Context, namespace string, labelSelector string) ([]corev1.PersistentVolumeClaim, error)
	Create(ctx context.Context, namespace string, pvc *corev1.PersistentVolumeClaim) error
}

type clientGoPVCClient struct {
	c kubernetes.Interface
}

func (k clientGoPVCClient) List(ctx context.Context, namespace, selector string) ([]corev1.PersistentVolumeClaim, error) {
	list, err := k.c.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k clientGoPVCClient) Create(ctx context.Context, namespace string, pvc *corev1.PersistentVolumeClaim) error {
	_, err := k.c.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
	return err
}

type pvcGroupPlugin struct {
	cfg     pvcGroupConfig
	client  pvcClient
	storage resource.Quantity
}

func newPVCGroupPlugin(cfg pvcGroupConfig, client pvcClient) (*pvcGroupPlugin, error) {
	cfg = cfg.withDefaults()
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("pvc-group: namespace is required")
	}
	q, err := resource.ParseQuantity(cfg.Size)
	if err != nil {
		return nil, fmt.Errorf("pvc-group: invalid size %q: %w", cfg.Size, err)
	}
	return &pvcGroupPlugin{cfg: cfg, client: client, storage: q}, nil
}

func (p *pvcGroupPlugin) Name() string { return "pvc-group" }

// Match returns true for entries whose objectClass list contains the
// configured class (default groupOfNames). The objectClass attribute may
// arrive as []string, []interface{}, or a bare string after JSON decoding;
// all three shapes are handled.
func (p *pvcGroupPlugin) Match(e SyncEvent) bool {
	if e.Op != SyncOpCreated && e.Op != SyncOpUpdated {
		return false
	}
	raw, ok := e.Content["objectClass"]
	if !ok {
		return false
	}
	want := strings.ToLower(p.cfg.MatchObjectClass)
	switch v := raw.(type) {
	case string:
		return strings.EqualFold(v, want)
	case []string:
		for _, oc := range v {
			if strings.EqualFold(oc, want) {
				return true
			}
		}
	case []interface{}:
		for _, oc := range v {
			if s, ok := oc.(string); ok && strings.EqualFold(s, want) {
				return true
			}
		}
	}
	return false
}

// groupNameFromContent extracts the canonical group short name. Prefers the
// cn attribute on the entry; falls back to parsing the leading cn= component
// of the DN if cn is missing or unusable.
func groupNameFromContent(dn string, content map[string]interface{}) string {
	if raw, ok := content["cn"]; ok {
		switch v := raw.(type) {
		case string:
			if v != "" {
				return v
			}
		case []string:
			if len(v) > 0 && v[0] != "" {
				return v[0]
			}
		case []interface{}:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	// Fallback: parse the first RDN, expecting cn=<value>,...
	first := strings.SplitN(dn, ",", 2)[0]
	if eq := strings.Index(first, "="); eq != -1 {
		return strings.TrimSpace(first[eq+1:])
	}
	return ""
}

// pvcNameSanitizer reduces an arbitrary string to a valid RFC 1123
// subdomain segment: lowercase, alphanumeric or '-', length-bounded,
// stripped of leading/trailing dashes, with runs of dashes collapsed.
var pvcInvalidRun = regexp.MustCompile(`[^a-z0-9]+`)

func sanitizePVCName(prefix, name string) string {
	combined := strings.ToLower(prefix + name)
	combined = pvcInvalidRun.ReplaceAllString(combined, "-")
	combined = strings.Trim(combined, "-")
	if combined == "" {
		combined = "group"
	}
	if len(combined) > 253 {
		combined = strings.TrimRight(combined[:253], "-")
	}
	return combined
}

func (p *pvcGroupPlugin) Apply(ctx context.Context, e SyncEvent) error {
	groupName := groupNameFromContent(e.DN, e.Content)
	if groupName == "" {
		return fmt.Errorf("pvc-group: cannot determine group name for DN %q", e.DN)
	}

	selector := fmt.Sprintf("%s=%s", p.cfg.GroupLabelKey, groupName)
	existing, err := p.client.List(ctx, p.cfg.Namespace, selector)
	if err != nil {
		return fmt.Errorf("pvc-group: list by label %q: %w", selector, err)
	}
	if len(existing) > 0 {
		if logger != nil {
			names := make([]string, len(existing))
			for i, pvc := range existing {
				names[i] = pvc.Name
			}
			logger.Debug("pvc-group: PVC already exists for group; skipping",
				"Group", groupName,
				"Namespace", p.cfg.Namespace,
				"PVCs", names,
			)
		}
		return nil
	}

	pvc := p.buildPVC(groupName)
	if err := p.client.Create(ctx, p.cfg.Namespace, pvc); err != nil {
		// AlreadyExists from a name collision is a benign race: another
		// dispatch raced to the same sanitized name. The label-based
		// listing above will catch the duplicate on the next sync.
		if apierrors.IsAlreadyExists(err) {
			if logger != nil {
				logger.Debug("pvc-group: PVC name already exists (benign race)",
					"Group", groupName,
					"Name", pvc.Name,
				)
			}
			return nil
		}
		return fmt.Errorf("pvc-group: create PVC %q: %w", pvc.Name, err)
	}
	if logger != nil {
		logger.Info("pvc-group: created PVC for group",
			"Group", groupName,
			"Namespace", p.cfg.Namespace,
			"Name", pvc.Name,
		)
	}
	return nil
}

func (p *pvcGroupPlugin) buildPVC(groupName string) *corev1.PersistentVolumeClaim {
	labels := map[string]string{
		p.cfg.GroupLabelKey: groupName,
	}
	for k, v := range p.cfg.ExtraLabels {
		labels[k] = v
	}

	modes := make([]corev1.PersistentVolumeAccessMode, 0, len(p.cfg.AccessModes))
	for _, m := range p.cfg.AccessModes {
		modes = append(modes, corev1.PersistentVolumeAccessMode(m))
	}

	spec := corev1.PersistentVolumeClaimSpec{
		AccessModes: modes,
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: p.storage,
			},
		},
	}
	if p.cfg.StorageClass != "" {
		sc := p.cfg.StorageClass
		spec.StorageClassName = &sc
	}

	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sanitizePVCName(p.cfg.NamePrefix, groupName),
			Namespace: p.cfg.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}
}

// buildKubeClient returns an in-cluster client when running inside a pod, or
// falls back to the supplied kubeconfig path (or ~/.kube/config) for local
// development.
func buildKubeClient(kubeconfig string) (kubernetes.Interface, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	}
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("pvc-group: build kube config from %q: %w", kubeconfig, err)
	}
	return kubernetes.NewForConfig(cfg)
}
