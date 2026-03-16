package controller

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	controlv1 "github.com/r7sa/k8s-routersync/api/v1"
)

const (
	deploymentName = "test-app"
)

func newMockServer(scheduleIsActive *bool) *httptest.Server {
	// [{"show":{"schedule":{"name":"schedule123"}}}]
	reSchedule := regexp.MustCompile("{\"show\":{\"schedule\":{\"name\":\"(.*)\"}}}")

	return httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth" {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == "GET" {
						// login challenge
						w.Write([]byte(`{"challenge": "12345", "status": "ok"}`))
					} else {
						// login response
						w.Write([]byte(`{"status": "ok"}`))
					}
					return
				}
				if r.URL.Path == "/rci/" && r.Method == "POST" {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, "Can't read body", http.StatusBadRequest)
						return
					}
					defer r.Body.Close()

					m := reSchedule.FindStringSubmatch(string(body))
					if len(m) <= 1 {
						w.Header().Set("Content-Type", "application/json")
						w.Write([]byte(`{"error":"schedule does not found"}`))

					} else {
						w.Header().Set("Content-Type", "application/json")
						lastType := "start"
						if *scheduleIsActive {
							lastType = "stop"
						}
						w.Write([]byte(
							fmt.Sprintf(`[{"show":{"schedule":{"%s":{"action":[
								{"type": "start","left": 586807,"dow": "Sun","time": "16:30"},
								{"type": "%s","left": 5407,"next": true,"dow": "Sun","time": "23:00"}
							]}}}}]`, m[1], lastType),
						))
					}
					return
				}
				w.WriteHeader(http.StatusNotFound)
			},
		),
	)
}

var _ = Describe("RouterSync Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		var mockRouterServer *httptest.Server

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		routersync := &controlv1.RouterSync{}
		scheduleIsActive := false

		BeforeEach(func() {
			mockRouterServer = newMockServer(&scheduleIsActive)
			mockRouterAddress := mockRouterServer.Listener.Addr().(*net.TCPAddr)

			By("creating the Secret first")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "netcraze-auth", Namespace: "default"},
				Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("pass")},
			}
			_ = k8sClient.Create(ctx, secret)

			By("creating a target Deployment with the required label")
			testDep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: "default",
					Labels: map[string]string{
						"routersync.r7sa.github.io/auto-scale-schedule":     "WorkHours",
						"routersync.r7sa.github.io/auto-scale-enabled":      "true",
						"routersync.r7sa.github.io/auto-scale-off-replicas": "1",
						"routersync.r7sa.github.io/auto-scale-on-replicas":  "10",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(5)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "test",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "web", Image: "registry.k8s.io/pause:3.9"}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, testDep)).To(Succeed())

			By("creating the custom resource for the Kind RouterSync")
			err := k8sClient.Get(ctx, typeNamespacedName, routersync)
			if err != nil && errors.IsNotFound(err) {
				resource := &controlv1.RouterSync{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: controlv1.RouterSyncSpec{
						RouterAddress: "127.0.0.1",
						RouterPort:    mockRouterAddress.Port,
						SecretRef:     "netcraze-auth",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &controlv1.RouterSync{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance RouterSync")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Cleanup the deployment")
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName, Namespace: "default"}}
			_ = k8sClient.Delete(ctx, dep)

			By("Cleanup the secret")
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "netcraze-auth", Namespace: "default"}}
			_ = k8sClient.Delete(ctx, sec)

			By("Stopping mock server")
			mockRouterServer.Close()
		})

		DescribeTable("Check reconcile",
			func(schedActive bool, expectedReplicas int) {
				scheduleIsActive = schedActive

				By("Reconciling the created resource")
				controllerReconciler := &RouterSyncReconciler{
					Client: k8sClient, Scheme: k8sClient.Scheme(),
				}
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				Expect(err).NotTo(HaveOccurred())

				By("Checking if the resource was reconciled successfully")
				updatedResource := &controlv1.RouterSync{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, updatedResource)).To(Succeed())
				Expect(updatedResource.Status.LastCheck).NotTo(BeEmpty())

				deployments := &appsv1.DeploymentList{}
				Expect(k8sClient.List(
					ctx, deployments,
					client.InNamespace("default"),
					client.MatchingLabels{"routersync.r7sa.github.io/auto-scale-enabled": "true"},
				)).To(Succeed())
				Expect(deployments.Items).Should(HaveLen(1))
				Expect(deployments.Items[0].Spec.Replicas).ToNot(BeNil())
				Expect(*deployments.Items[0].Spec.Replicas).To(Equal(int32(expectedReplicas)))
			},
			Entry("scheduling is active", true, 10),
			Entry("scheduling is not active", false, 1),
		)
	})
})
