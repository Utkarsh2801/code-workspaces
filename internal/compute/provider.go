package compute

import (
	"context"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var ErrNotFound = errors.New("instance not found")

const StateRunning = "running"

// contains information about a machine instance.
type Instance struct {
	ID        string
	Name      string
	UID       string
	State     string // running, stopped
	IP        string
	URL       string
	CreatedAt time.Time
}

// Provider needs and createRequest to create machine instances.
type CreateRequest struct {
	UID            string
	Name           string
	Namespace      string
	Owner          string
	Image          string
	Port           int
	Args           []string
	StorageSize    string
	OwnerReference metav1.OwnerReference
}

type Provider interface {
	Create(ctx context.Context, req CreateRequest) (*Instance, error)

	Delete(ctx context.Context, id string) error

	Get(ctx context.Context, id string) (*Instance, error)

	List(ctx context.Context) ([]*Instance, error)
}
