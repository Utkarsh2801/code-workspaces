package compute

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	workspaceContainerName  = "workspace"
	workspaceMountPath      = "/workspace"
	workspaceSpecAnnotation = "workspaces.example.com/spec-hash"
	defaultStorageSize      = "1Gi"
	setupContainerName      = "install-node"
	setupMarkerPath         = workspaceMountPath + "/.node-22-installed"
	setupScript             = `#!/bin/sh
set -eu

TOOLS_DIR=/workspace/tools/node
MARKER=/workspace/.node-22-installed

if [ -f "$MARKER" ] && [ -x "$TOOLS_DIR/bin/node" ]; then
  "$TOOLS_DIR/bin/node" --version
  exit 0
fi

mkdir -p "$TOOLS_DIR/bin"
cp -f /usr/local/bin/node "$TOOLS_DIR/bin/node"

"$TOOLS_DIR/bin/node" --version
: > "$MARKER"
`
)

// KubernetesProvider creates workspace containers as Kubernetes Pods.
type KubernetesProvider struct {
	client client.Client
}

func NewKubernetesProvider(k8sClient client.Client) (*KubernetesProvider, error) {
	if k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}
	return &KubernetesProvider{client: k8sClient}, nil
}

func (p *KubernetesProvider) Create(ctx context.Context, req CreateRequest) (*Instance, error) {
	if req.StorageSize == "" {
		req.StorageSize = defaultStorageSize
	}
	storageQuantity, err := resource.ParseQuantity(req.StorageSize)
	if err != nil {
		return nil, fmt.Errorf("parse workspace storage size %q: %w", req.StorageSize, err)
	}

	labels := map[string]string{
		LabelManagedBy: managedValue,
		LabelUID:       req.UID,
		LabelOwner:     ownerLabelValue(req.Owner),
	}

	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	if err := p.client.Get(ctx, pvcKey, pvc); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get workspace PVC %s/%s: %w", req.Namespace, req.Name, err)
		}
		pvcOwnerReferences := []metav1.OwnerReference{req.OwnerReference}
		if req.RetainStorage {
			pvcOwnerReferences = nil
		}
		pvc = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: req.Namespace, Labels: labels, OwnerReferences: pvcOwnerReferences},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storageQuantity}},
			},
		}
		if err := p.client.Create(ctx, pvc); err != nil {
			return nil, fmt.Errorf("create workspace PVC %s/%s: %w", req.Namespace, req.Name, err)
		}
	}

	specHash := workspaceSpecHash(req)
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}
	if err := p.client.Get(ctx, podKey, pod); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get workspace Pod %s/%s: %w", req.Namespace, req.Name, err)
	}

	podExists := pod.Name != ""
	if podExists && pod.Annotations[workspaceSpecAnnotation] == specHash {
		return p.Get(ctx, req.UID)
	}
	if podExists {
		if err := p.client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("replace workspace Pod %s/%s: %w", req.Namespace, req.Name, err)
		}
	}

	allowPrivilegeEscalation := false
	runAsNonRoot := true
	setupRunAsUser := int64(1000)
	pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            req.Name,
			Namespace:       req.Namespace,
			Labels:          labels,
			Annotations:     map[string]string{workspaceSpecAnnotation: specHash},
			OwnerReferences: []metav1.OwnerReference{req.OwnerReference},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			InitContainers: []corev1.Container{{
				Name:            setupContainerName,
				Image:           "node:22-slim",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/sh", "-c", setupScript},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					RunAsNonRoot:             &runAsNonRoot,
					RunAsUser:                &setupRunAsUser,
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "workspace-data", MountPath: workspaceMountPath}},
			}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &runAsNonRoot,
				RunAsUser:      &setupRunAsUser,
				FSGroup:        &setupRunAsUser,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            workspaceContainerName,
				Image:           req.Image,
				Args:            req.Args,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env:             []corev1.EnvVar{{Name: "PATH", Value: "/workspace/tools/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
				Ports:        []corev1.ContainerPort{{Name: "http", ContainerPort: int32(req.Port), Protocol: corev1.ProtocolTCP}},
				VolumeMounts: []corev1.VolumeMount{{Name: "workspace-data", MountPath: workspaceMountPath}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "workspace-data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: req.Name}},
			}},
		},
	}

	if err := p.client.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create workspace Pod %s/%s: %w", req.Namespace, req.Name, err)
	}
	return p.Get(ctx, req.UID)
}

func workspaceSpecHash(req CreateRequest) string {
	args := fmt.Sprintf("%v", req.Args)
	hash := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%s|%s|%t", req.Image, req.Port, args, req.StorageSize, req.RetainStorage))
	return fmt.Sprintf("%x", hash[:])
}

func ownerLabelValue(owner string) string {
	var value strings.Builder
	for _, char := range strings.ToLower(owner) {
		switch {
		case char == '@':
			value.WriteString("_at_")
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '_', char == '.':
			value.WriteRune(char)
		default:
			value.WriteByte('-')
		}
	}

	label := strings.Trim(value.String(), "-_.")
	if len(label) > 63 {
		label = strings.TrimRight(label[:63], "-_.")
	}
	return label
}

func (p *KubernetesProvider) Get(ctx context.Context, uid string) (*Instance, error) {
	pods := &corev1.PodList{}
	if err := p.client.List(ctx, pods, client.MatchingLabels{LabelUID: uid}); err != nil {
		return nil, fmt.Errorf("list workspace Pods for UID %s: %w", uid, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("workspace for %s: %w", uid, ErrNotFound)
	}

	pod := &pods.Items[0]
	for i := range pod.Status.InitContainerStatuses {
		status := pod.Status.InitContainerStatuses[i]
		if status.Name != setupContainerName {
			continue
		}
		if status.State.Terminated != nil {
			if status.State.Terminated.ExitCode != 0 {
				return &Instance{ID: string(pod.UID), Name: pod.Name, UID: uid, State: StateFailed, IP: pod.Status.PodIP, CreatedAt: pod.CreationTimestamp.Time}, nil
			}
		} else if status.State.Waiting != nil {
			switch status.State.Waiting.Reason {
			case "CreateContainerConfigError", "RunContainerError", "CrashLoopBackOff":
				return &Instance{ID: string(pod.UID), Name: pod.Name, UID: uid, State: StateFailed, IP: pod.Status.PodIP, CreatedAt: pod.CreationTimestamp.Time}, nil
			default:
				return &Instance{ID: string(pod.UID), Name: pod.Name, UID: uid, State: StateCreating, IP: pod.Status.PodIP, CreatedAt: pod.CreationTimestamp.Time}, nil
			}
		} else {
			return &Instance{ID: string(pod.UID), Name: pod.Name, UID: uid, State: StateCreating, IP: pod.Status.PodIP, CreatedAt: pod.CreationTimestamp.Time}, nil
		}
	}

	state := string(pod.Status.Phase)
	if pod.Status.Phase == corev1.PodRunning {
		state = StateRunning
	} else if state == "" {
		state = "Pending"
	}

	return &Instance{
		ID:        string(pod.UID),
		Name:      pod.Name,
		UID:       uid,
		State:     state,
		IP:        pod.Status.PodIP,
		CreatedAt: pod.CreationTimestamp.Time,
	}, nil
}

func (p *KubernetesProvider) Delete(ctx context.Context, req DeleteRequest) error {
	pods := &corev1.PodList{}
	if err := p.client.List(ctx, pods, client.MatchingLabels{LabelUID: req.UID}); err != nil {
		return fmt.Errorf("list workspace Pods for UID %s: %w", req.UID, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := p.client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete workspace Pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}

		if req.RetainStorage {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
		if err := p.client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete workspace PVC %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

func (p *KubernetesProvider) List(ctx context.Context) ([]*Instance, error) {
	pods := &corev1.PodList{}
	if err := p.client.List(ctx, pods, client.MatchingLabels{LabelManagedBy: managedValue}); err != nil {
		return nil, fmt.Errorf("list managed workspace Pods: %w", err)
	}

	instances := make([]*Instance, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		state := string(pod.Status.Phase)
		if pod.Status.Phase == corev1.PodRunning {
			state = StateRunning
		} else if state == "" {
			state = "Pending"
		}
		instances = append(instances, &Instance{
			ID:        string(pod.UID),
			Name:      pod.Name,
			UID:       pod.Labels[LabelUID],
			State:     state,
			IP:        pod.Status.PodIP,
			CreatedAt: pod.CreationTimestamp.Time,
		})
	}
	return instances, nil
}

var _ Provider = (*KubernetesProvider)(nil)
