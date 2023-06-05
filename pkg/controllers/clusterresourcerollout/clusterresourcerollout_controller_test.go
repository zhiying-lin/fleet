/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package clusterresourcerollout

import (
	"fmt"
	fleetv1 "go.goms.io/fleet/apis/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

const (
	testClusterResourcePlacementName = "my-crp"
)

func resourceSnapshotForTest(index int) *fleetv1.ClusterResourceSnapshot {
	return &fleetv1.ClusterResourceSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf(fleetv1.ResourceSnapshotNameFmt, testClusterResourcePlacementName, index),
		},
		Spec:   fleetv1.ResourceSnapshotSpec{

		}
	}
}

func TestHandleUpdate(t *testing.T) {

}
