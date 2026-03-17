package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RouterSyncSpec struct {
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// Router type (e.g. netcraze, etc. - for future extensibility)
	RouterType string `json:"routerType"`

	// Router address in format "host:port"
	RouterAddress string `json:"routerAddress"`

	// Secret with Router username and password
	SecretRef string `json:"secretRef"`
}

// RouterSyncStatus defines the observed state of RouterSync.
type RouterSyncStatus struct {
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	LastCheck string `json:"lastCheck"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RouterSync is the Schema for the routersyncs API
type RouterSync struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RouterSync
	// +required
	Spec RouterSyncSpec `json:"spec"`

	// status defines the observed state of RouterSync
	// +optional
	Status RouterSyncStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RouterSyncList contains a list of RouterSync
type RouterSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RouterSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RouterSync{}, &RouterSyncList{})
}
