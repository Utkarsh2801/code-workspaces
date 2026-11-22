package compute

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testWorkspaceUID    = "workspace-uid"
	testWorkspaceName   = "test-workspace"
	testNamespaceName   = "default"
	testCodeServerImage = "codercom/code-server"
)

func newTestKubernetesProvider(t *testing.T, objects ...runtime.Object) *KubernetesProvider {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core API to scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	provider, err := NewKubernetesProvider(k8sClient)
	if err != nil {
		t.Fatalf("NewKubernetesProvider() error: %v", err)
	}
	return provider
}

func TestKubernetesProviderCreateGetAndDelete(t *testing.T) {
	provider := newTestKubernetesProvider(t)
	ctx := context.Background()

	instance, err := provider.Create(ctx, CreateRequest{
		UID:       testWorkspaceUID,
		Name:      testWorkspaceName,
		Namespace: testNamespaceName,
		Owner:     "owner@example.com",
		Image:     testCodeServerImage,
		Port:      8080,
		Args:      []string{"--bind-addr", "0.0.0.0:8080"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if instance.Name != testWorkspaceName {
		t.Errorf("Create() name = %q, want test-workspace", instance.Name)
	}

	var pod corev1.Pod
	if err := provider.client.Get(ctx, types.NamespacedName{Name: testWorkspaceName, Namespace: testNamespaceName}, &pod); err != nil {
		t.Fatalf("get created Pod: %v", err)
	}
	if pod.Labels[LabelUID] != testWorkspaceUID {
		t.Errorf("Pod UID label = %q, want workspace-uid", pod.Labels[LabelUID])
	}
	if pod.Spec.Containers[0].Image != testCodeServerImage {
		t.Errorf("Pod image = %q, want codercom/code-server", pod.Spec.Containers[0].Image)
	}
	container := pod.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("Pod container must disable privilege escalation")
	}
	hasNodePath := false
	for _, env := range container.Env {
		if env.Name == "PATH" && strings.Contains(env.Value, "/workspace/tools/node/bin") {
			hasNodePath = true
		}
	}
	if !hasNodePath {
		t.Errorf("Workspace container PATH = %+v, want /workspace/tools/node/bin", container.Env)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != setupContainerName {
		t.Errorf("Pod init containers = %+v, want %s", pod.Spec.InitContainers, setupContainerName)
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].MountPath != workspaceMountPath {
		t.Errorf("Pod volume mounts = %+v, want %s mount", container.VolumeMounts, workspaceMountPath)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := provider.client.Get(ctx, types.NamespacedName{Name: testWorkspaceName, Namespace: testNamespaceName}, &pvc); err != nil {
		t.Fatalf("get created PVC: %v", err)
	}
	if pvc.Spec.Resources.Requests.Storage().String() != defaultStorageSize {
		t.Errorf("PVC storage = %s, want %s", pvc.Spec.Resources.Requests.Storage().String(), defaultStorageSize)
	}

	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.4"
	if err := provider.client.Status().Update(ctx, &pod); err != nil {
		t.Fatalf("update Pod status: %v", err)
	}

	instance, err = provider.Get(ctx, testWorkspaceUID)
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	if instance.State != StateRunning || instance.IP != "10.0.0.4" {
		t.Errorf("Get() = %+v, want running Pod with IP 10.0.0.4", instance)
	}

	if err := provider.Delete(ctx, DeleteRequest{UID: testWorkspaceUID}); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	_, err = provider.Get(ctx, testWorkspaceUID)
	if err == nil {
		t.Fatal("Get() after Delete() error = nil, want ErrNotFound")
	}
	var deletedPVC corev1.PersistentVolumeClaim
	if err := provider.client.Get(ctx, types.NamespacedName{Name: testWorkspaceName, Namespace: testNamespaceName}, &deletedPVC); err == nil {
		t.Fatal("PVC remains after Delete(), want deleted PVC")
	}
}

func TestKubernetesProviderRetainsPVC(t *testing.T) {
	provider := newTestKubernetesProvider(t)
	ctx := context.Background()

	_, err := provider.Create(ctx, CreateRequest{
		UID: testWorkspaceUID, Name: testWorkspaceName, Namespace: testNamespaceName,
		Owner: "owner@example.com", Image: testCodeServerImage, Port: 8080,
		RetainStorage: true,
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if err := provider.Delete(ctx, DeleteRequest{UID: testWorkspaceUID, RetainStorage: true}); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := provider.client.Get(ctx, types.NamespacedName{Name: testWorkspaceName, Namespace: testNamespaceName}, &pvc); err != nil {
		t.Fatalf("retained PVC should exist: %v", err)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Errorf("retained PVC owner references = %+v, want none", pvc.OwnerReferences)
	}
}

func TestKubernetesProviderSanitizesOwnerLabel(t *testing.T) {
	provider := newTestKubernetesProvider(t)
	_, err := provider.Create(context.Background(), CreateRequest{
		UID: testWorkspaceUID, Name: testWorkspaceName, Namespace: testNamespaceName,
		Owner: "Alice.User+tag@example.com", Image: testCodeServerImage, Port: 8080,
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	var pod corev1.Pod
	if err := provider.client.Get(context.Background(), types.NamespacedName{Name: testWorkspaceName, Namespace: testNamespaceName}, &pod); err != nil {
		t.Fatalf("get Pod: %v", err)
	}
	if pod.Labels[LabelOwner] != "alice.user-tag_at_example.com" {
		t.Errorf("owner label = %q, want %q", pod.Labels[LabelOwner], "alice.user-tag_at_example.com")
	}
}

func TestKubernetesProviderInitSetupStates(t *testing.T) {
	tests := []struct {
		name       string
		terminated *corev1.ContainerStateTerminated
		waiting    *corev1.ContainerStateWaiting
		wantState  string
	}{
		{name: "completed", terminated: &corev1.ContainerStateTerminated{ExitCode: 0}, waiting: nil, wantState: "Running"},
		{name: "failed", terminated: &corev1.ContainerStateTerminated{ExitCode: 23}, waiting: nil, wantState: StateFailed},
		{name: "crash loop", terminated: nil, waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}, wantState: StateFailed},
		{name: "running", terminated: nil, waiting: nil, wantState: StateCreating},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testNamespaceName, Labels: map[string]string{LabelUID: testWorkspaceUID}},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{{
						Name:  setupContainerName,
						State: corev1.ContainerState{Terminated: tt.terminated, Waiting: tt.waiting},
					}},
				},
			}
			provider := newTestKubernetesProvider(t, pod)

			instance, err := provider.Get(context.Background(), testWorkspaceUID)
			if err != nil {
				t.Fatalf("Get() error: %v", err)
			}
			if tt.wantState == "Running" {
				pod.Status.Phase = corev1.PodRunning
				if err := provider.client.Status().Update(context.Background(), pod); err != nil {
					t.Fatalf("update Pod status: %v", err)
				}
				instance, err = provider.Get(context.Background(), testWorkspaceUID)
				if err != nil {
					t.Fatalf("Get() after running: %v", err)
				}
			}
			want := tt.wantState
			if tt.wantState == "Running" {
				want = StateRunning
			}
			if instance.State != want {
				t.Errorf("Get() state = %q, want %q", instance.State, want)
			}
		})
	}
}

func TestKubernetesProviderGetNotFound(t *testing.T) {
	provider := newTestKubernetesProvider(t)
	_, err := provider.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("Get() error = nil, want not-found error")
	}
}

func TestKubernetesProviderList(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testWorkspaceName,
			Namespace: testNamespaceName,
			Labels: map[string]string{
				LabelManagedBy: managedValue,
				LabelUID:       testWorkspaceUID,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5"},
	}
	provider := newTestKubernetesProvider(t, pod)

	instances, err := provider.List(context.Background())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("List() returned %d instances, want 1", len(instances))
	}
	if instances[0].UID != testWorkspaceUID || instances[0].State != StateRunning {
		t.Errorf("List()[0] = %+v, want running UID workspace-uid", instances[0])
	}
}
