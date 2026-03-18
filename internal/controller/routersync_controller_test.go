package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	controlv1 "github.com/r7sa/k8s-routersync/api/v1"
)

const (
	deploymentName = "test-app"
)

var scheduleResponse = map[bool]string{
	true: `schedule, name = WorkHours:
  action, type = start, left = 1234:
  action, type = stop, left = 123:
schedule, name = OffHours:
`,
	false: `schedule, name = WorkHours:
  action, type = stop, left = 1234:
  action, type = start, left = 123:
schedule, name = OffHours:
`,
}

func startMockSSH(responseKey *bool) (string, func()) {
	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "admin" && string(pass) == "pass" {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected")
		},
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	hostKey, _ := ssh.NewSignerFromKey(key)
	config.AddHostKey(hostKey)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// SSH Handshake
			sshConn, chans, reqs, _ := ssh.NewServerConn(conn, config)
			go ssh.DiscardRequests(reqs)
			go func(in <-chan ssh.NewChannel) {
				for newChan := range in {
					if newChan.ChannelType() != "session" {
						_ = newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
						continue
					}
					channel, requests, _ := newChan.Accept()
					go func(in <-chan *ssh.Request) {
						for req := range in {
							// Нам нужен тип запроса "exec" (это делает session.Run)
							if req.Type == "exec" {
								// Проверяем команду (первые 4 байта длины + сама команда)
								cmd := string(req.Payload[4:])
								if strings.Contains(cmd, "show schedule") {
									response := scheduleResponse[*responseKey]
									_, _ = channel.Write([]byte(response))
									_ = req.Reply(true, nil)
									// Важно: закрываем канал с кодом 0
									_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
									_ = channel.Close()
								}
							}
						}
					}(requests)
				}
			}(chans)
			_ = sshConn
		}
	}()

	return ln.Addr().String(), func() { _ = ln.Close() }
}

var _ = Describe("RouterSync Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		var mockRouterAddress string
		var mockRouterCloser func()

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		routersync := &controlv1.RouterSync{}
		scheduleIsActive := false

		BeforeEach(func() {
			mockRouterAddress, mockRouterCloser = startMockSSH(&scheduleIsActive)

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
						RouterAddress: mockRouterAddress,
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
			mockRouterCloser()
		})

		DescribeTable("Check reconcile",
			func(schedActive bool, expectedReplicas int) {
				scheduleIsActive = schedActive

				By("Reconciling the created resource")
				controllerReconciler := &RouterSyncReconciler{
					Client: k8sClient, Scheme: k8sClient.Scheme(),
					Recorder: events.NewFakeRecorder(100),
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

				fakeRecorder := controllerReconciler.Recorder.(*events.FakeRecorder)
				select {
				case event := <-fakeRecorder.Events:
					Expect(event).To(ContainSubstring("Scaled"))
				default:
					Fail("No events was recorder into Recorder")
				}
			},
			Entry("scheduling is active", true, 10),
			Entry("scheduling is not active", false, 1),
		)
	})
})
