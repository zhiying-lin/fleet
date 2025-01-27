/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/
package utils

import (
	"context"
	"embed"
	"fmt"
	"time"

	// Lint check prohibits non "_test" ending files to have dot imports for ginkgo / gomega.
	"github.com/google/go-cmp/cmp"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"
	workapi "sigs.k8s.io/work-api/pkg/apis/v1alpha1"

	fleetv1alpha1 "go.goms.io/fleet/apis/v1alpha1"
	"go.goms.io/fleet/pkg/utils"
	"go.goms.io/fleet/test/e2e/framework"
)

const (
	testClusterRole        = "wh-test-cluster-role"
	testClusterRoleBinding = "wh-test-cluster-role-binding"
	testRole               = "wh-test-role"
	testRoleBinding        = "wh-test-role-binding"
)

var (
	// PollInterval defines the interval time for a poll operation.
	PollInterval = 250 * time.Millisecond
	// PollTimeout defines the time after which the poll operation times out.
	PollTimeout = 60 * time.Second
)

// DeleteMemberCluster deletes MemberCluster in the hub cluster.
func DeleteMemberCluster(ctx context.Context, cluster framework.Cluster, mc *fleetv1alpha1.MemberCluster) {
	gomega.Expect(cluster.KubeClient.Delete(ctx, mc)).Should(gomega.Succeed(), "Failed to delete member cluster %s in %s cluster", mc.Name, cluster.ClusterName)
	gomega.Eventually(func() bool {
		return apierrors.IsNotFound(cluster.KubeClient.Get(ctx, types.NamespacedName{Name: mc.Name}, mc))
	}, PollTimeout, PollInterval).Should(gomega.BeTrue(), "Failed to wait for member cluster %s to be deleted in %s cluster", mc.Name, cluster.ClusterName)
}

// CheckMemberClusterStatus is used to check member cluster status.
func CheckMemberClusterStatus(ctx context.Context, cluster framework.Cluster, objectKey *types.NamespacedName, wantMCStatus fleetv1alpha1.MemberClusterStatus, mcStatusCmpOptions []cmp.Option) {
	gotMC := &fleetv1alpha1.MemberCluster{}
	gomega.Eventually(func() error {
		if err := cluster.KubeClient.Get(ctx, types.NamespacedName{Name: objectKey.Name}, gotMC); err != nil {
			return err
		}
		if statusDiff := cmp.Diff(wantMCStatus, gotMC.Status, mcStatusCmpOptions...); statusDiff != "" {
			return fmt.Errorf("member cluster(%s) status mismatch (-want +got):\n%s", gotMC.Name, statusDiff)
		}
		return nil
	}, PollTimeout, PollInterval).Should(gomega.Succeed(), "Failed to wait member cluster %s to have status %s", gotMC.Name, wantMCStatus)
}

// CheckInternalMemberClusterStatus is used to check internal member cluster status.
func CheckInternalMemberClusterStatus(ctx context.Context, cluster framework.Cluster, objectKey *types.NamespacedName, wantIMCStatus fleetv1alpha1.InternalMemberClusterStatus, imcStatusCmpOptions []cmp.Option) {
	gotIMC := &fleetv1alpha1.InternalMemberCluster{}
	gomega.Eventually(func() error {
		if err := cluster.KubeClient.Get(ctx, types.NamespacedName{Name: objectKey.Name, Namespace: objectKey.Namespace}, gotIMC); err != nil {
			return err
		}
		if statusDiff := cmp.Diff(wantIMCStatus, gotIMC.Status, imcStatusCmpOptions...); statusDiff != "" {
			return fmt.Errorf("member cluster(%s) status mismatch (-want +got):\n%s", gotIMC.Name, statusDiff)
		}
		return nil
	}, PollTimeout, PollInterval).Should(gomega.Succeed(), "Failed to wait for internal member cluster %s to have status %s", gotIMC.Name, wantIMCStatus)
}

// WaitWork waits for Work to be present on the hub cluster.
func WaitWork(ctx context.Context, cluster framework.Cluster, workName, workNamespace string) {
	name := types.NamespacedName{Name: workName, Namespace: workNamespace}

	klog.Infof("Waiting for Work(%s/%s) to be synced", workName, workNamespace)
	gomega.Eventually(func() error {
		var work workapi.Work

		return cluster.KubeClient.Get(ctx, name, &work)
	}, PollTimeout, PollInterval).Should(gomega.Succeed(), "Work %s not synced", name)
}

// DeleteNamespace delete namespace.
func DeleteNamespace(ctx context.Context, cluster framework.Cluster, ns *corev1.Namespace) {
	gomega.Expect(cluster.KubeClient.Delete(context.TODO(), ns)).Should(gomega.Succeed(), "Failed to delete namespace %s in %s cluster", ns.Name, cluster.ClusterName)
	gomega.Eventually(func() bool {
		return apierrors.IsNotFound(cluster.KubeClient.Get(ctx, types.NamespacedName{Name: ns.Name}, ns))
	}, PollTimeout, PollInterval).Should(gomega.BeTrue(), "Failed to wait for namespace %s to be deleted in %s cluster", ns.Name, cluster.ClusterName)
}

// CreateWork creates Work object based on manifest given.
func CreateWork(ctx context.Context, hubCluster framework.Cluster, workName, workNamespace string, manifests []workapi.Manifest) workapi.Work {
	work := workapi.Work{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workName,
			Namespace: workNamespace,
		},
		Spec: workapi.WorkSpec{
			Workload: workapi.WorkloadTemplate{
				Manifests: manifests,
			},
		},
	}

	err := hubCluster.KubeClient.Create(ctx, &work)
	gomega.Expect(err).Should(gomega.Succeed(), "Failed to create work %s in namespace %v", workName, workNamespace)
	return work
}

// UpdateWork updates an existing Work Object by replacing the Spec.Manifest with a new objects given from parameter.
func UpdateWork(ctx context.Context, hubCluster *framework.Cluster, work *workapi.Work, objects []runtime.Object) *workapi.Work {
	manifests := make([]workapi.Manifest, len(objects))
	for index, obj := range objects {
		rawObj, err := json.Marshal(obj)
		gomega.Expect(err).Should(gomega.Succeed(), "Failed to marshal object %+v", obj)

		manifests[index] = workapi.Manifest{
			RawExtension: runtime.RawExtension{Object: obj, Raw: rawObj},
		}
	}
	work.Spec.Workload.Manifests = manifests

	err := hubCluster.KubeClient.Update(ctx, work)
	gomega.Expect(err).Should(gomega.Succeed(), "Failed to update work %s in namespace %v", work.Name, work.Namespace)

	return work
}

// DeleteWork deletes the given Work object and waits until work becomes not found.
func DeleteWork(ctx context.Context, hubCluster framework.Cluster, work workapi.Work) {
	// Deleting Work
	gomega.Expect(hubCluster.KubeClient.Delete(ctx, &work)).Should(gomega.Succeed(), "Deletion of work %s failed", work.Name)

	// Waiting for the Work to be deleted and not found.
	gomega.Eventually(func() error {
		namespaceType := types.NamespacedName{Name: work.Name, Namespace: work.Namespace}
		return hubCluster.KubeClient.Get(ctx, namespaceType, &work)
	}).Should(&utils.NotFoundMatcher{},
		"The Work resource %s was not deleted", work.Name, hubCluster.ClusterName)
}

// AddManifests adds manifests to be included within a Work.
func AddManifests(objects []runtime.Object, manifests []workapi.Manifest) []workapi.Manifest {
	for _, obj := range objects {
		rawObj, err := json.Marshal(obj)
		gomega.Expect(err).Should(gomega.Succeed(), "Failed to marshal object %+v", obj)
		manifests = append(manifests, workapi.Manifest{
			RawExtension: runtime.RawExtension{Object: obj, Raw: rawObj},
		})
	}
	return manifests
}

// AddByteArrayToManifest adds a given ByteArray to the manifest for Work Object.
func AddByteArrayToManifest(bytes []byte, manifests []workapi.Manifest) []workapi.Manifest {
	return append(manifests, workapi.Manifest{RawExtension: runtime.RawExtension{Raw: bytes}})
}

// RandomWorkName creates a work name in a correct format for e2e tests.
func RandomWorkName(length int) string {
	return "work" + rand.String(length)
}

// GenerateCRDObjectFromFile provides the object and gvk from the manifest file given.
func GenerateCRDObjectFromFile(cluster framework.Cluster, fs embed.FS, filepath string, genericCodec runtime.Decoder) (runtime.Object, *schema.GroupVersionKind, schema.GroupVersionResource) {
	fileRaw, err := fs.ReadFile(filepath)
	gomega.Expect(err).Should(gomega.Succeed(), "Reading manifest file %s failed", filepath)

	obj, gvk, err := genericCodec.Decode(fileRaw, nil, nil)
	gomega.Expect(err).Should(gomega.Succeed(), "Decoding manifest file %s failed", filepath)

	jsonObj, err := json.Marshal(obj)
	gomega.Expect(err).Should(gomega.Succeed(), "Marshalling failed for file %s", filepath)

	newObj := &unstructured.Unstructured{}
	gomega.Expect(newObj.UnmarshalJSON(jsonObj)).Should(gomega.Succeed(),
		"Unmarshalling failed for object %s", newObj)

	mapping, err := cluster.RestMapper.RESTMapping(newObj.GroupVersionKind().GroupKind(), newObj.GroupVersionKind().Version)
	gomega.Expect(err).Should(gomega.Succeed(), "CRD data was not mapped in the restMapper")

	return obj, gvk, mapping.Resource
}

// CreateResourcesForWebHookE2E create resources required for Webhook E2E.
func CreateResourcesForWebHookE2E(ctx context.Context, hubCluster *framework.Cluster, memberNamespace string) {
	cr := rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterRole,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"*"},
				Verbs:     []string{"*"},
				Resources: []string{"*"},
			},
		},
	}
	gomega.Eventually(func() error {
		return hubCluster.KubeClient.Create(ctx, &cr)
	}, PollTimeout, PollInterval).Should(gomega.Succeed(), "failed to create cluster role %s for webhook E2E", cr.Name)

	crb := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterRoleBinding,
		},
		Subjects: []rbacv1.Subject{
			{
				APIGroup: rbacv1.GroupName,
				Kind:     "User",
				Name:     "test-user",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     testClusterRole,
		},
	}

	gomega.Eventually(func() error {
		return hubCluster.KubeClient.Create(ctx, &crb)
	}, PollTimeout, PollInterval).Should(gomega.Succeed(), "failed to create cluster role binding %s for webhook E2E", crb.Name)

	r := rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRole,
			Namespace: memberNamespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"*"},
				APIGroups: []string{"*"},
				Resources: []string{"*"},
			},
		},
	}
	gomega.Eventually(func() error {
		return hubCluster.KubeClient.Create(ctx, &r)
	}, PollTimeout, PollInterval).Should(gomega.Succeed(), "failed to create role %s for webhook E2E", r.Name)

	rb := rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRoleBinding,
			Namespace: memberNamespace,
		},
		Subjects: []rbacv1.Subject{
			{
				APIGroup: rbacv1.GroupName,
				Kind:     "User",
				Name:     "test-user",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     testRole,
		},
	}
	gomega.Eventually(func() error {
		return hubCluster.KubeClient.Create(ctx, &rb)
	}, PollTimeout, PollInterval).Should(gomega.Succeed(), "failed to create role binding %s for webhook E2E", rb.Name)
}

// DeleteResourcesForWebHookE2E deletes resources created for Webhook E2E.
func DeleteResourcesForWebHookE2E(ctx context.Context, hubCluster *framework.Cluster, memberNamespace string) {
	rb := rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRoleBinding,
			Namespace: memberNamespace,
		},
	}
	gomega.Expect(hubCluster.KubeClient.Delete(ctx, &rb)).Should(gomega.Succeed())

	r := rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRole,
			Namespace: memberNamespace,
		},
	}
	gomega.Expect(hubCluster.KubeClient.Delete(ctx, &r)).Should(gomega.Succeed())

	crb := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterRoleBinding,
		},
	}
	gomega.Expect(hubCluster.KubeClient.Delete(ctx, &crb)).Should(gomega.Succeed())

	cr := rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterRole,
		},
	}
	gomega.Expect(hubCluster.KubeClient.Delete(ctx, &cr)).Should(gomega.Succeed())
}
