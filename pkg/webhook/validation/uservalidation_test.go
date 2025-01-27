package validation

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.goms.io/fleet/pkg/utils"
)

func TestValidateUserForResource(t *testing.T) {
	testCases := map[string]struct {
		resKind          string
		namespacedName   types.NamespacedName
		whiteListedUsers []string
		userInfo         v1.UserInfo
		wantResponse     admission.Response
	}{
		"allow user in system:masters group": {
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{mastersGroup},
			},
			resKind:        "Role",
			namespacedName: types.NamespacedName{Name: "test-role", Namespace: "test-namespace"},
			wantResponse:   admission.Allowed(fmt.Sprintf(fleetResourceAllowedFormat, "test-user", []string{mastersGroup}, "Role", types.NamespacedName{Name: "test-role", Namespace: "test-namespace"})),
		},
		"allow white listed user not in system:masters group": {
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{"test-group"},
			},
			resKind:          "RoleBinding",
			namespacedName:   types.NamespacedName{Name: "test-role-binding", Namespace: "test-namespace"},
			whiteListedUsers: []string{"test-user"},
			wantResponse:     admission.Allowed(fmt.Sprintf(fleetResourceAllowedFormat, "test-user", []string{"test-group"}, "RoleBinding", types.NamespacedName{Name: "test-role-binding", Namespace: "test-namespace"})),
		},
		"allow valid service account": {
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{serviceAccountsGroup},
			},
			resKind:        "RoleBinding",
			namespacedName: types.NamespacedName{Name: "test-role-binding", Namespace: "test-namespace"},
			wantResponse:   admission.Allowed(fmt.Sprintf(fleetResourceAllowedFormat, "test-user", []string{serviceAccountsGroup}, "RoleBinding", types.NamespacedName{Name: "test-role-binding", Namespace: "test-namespace"})),
		},
		"fail to validate user with invalid username, groups": {
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{"test-group"},
			},
			resKind:        "Role",
			namespacedName: types.NamespacedName{Name: "test-role", Namespace: "test-namespace"},
			wantResponse:   admission.Denied(fmt.Sprintf(fleetResourceDeniedFormat, "test-user", []string{"test-group"}, "Role", types.NamespacedName{Name: "test-role", Namespace: "test-namespace"})),
		},
	}

	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			gotResult := ValidateUserForResource(testCase.resKind, testCase.namespacedName, testCase.whiteListedUsers, testCase.userInfo)
			assert.Equal(t, testCase.wantResponse, gotResult, utils.TestCaseMsg, testName)
		})
	}
}

func TestValidateUserForFleetCRD(t *testing.T) {
	testCases := map[string]struct {
		group            string
		namespacedName   types.NamespacedName
		whiteListedUsers []string
		userInfo         v1.UserInfo
		wantResponse     admission.Response
	}{
		"allow user in system:masters group to modify other CRD": {
			group:          "other-group",
			namespacedName: types.NamespacedName{Name: "test-crd"},
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{"system:masters"},
			},
			wantResponse: admission.Allowed(fmt.Sprintf(crdAllowedFormat, "test-user", []string{mastersGroup}, types.NamespacedName{Name: "test-crd"})),
		},
		"allow user in system:masters group to modify fleet CRD": {
			group:          "fleet.azure.com",
			namespacedName: types.NamespacedName{Name: "memberclusters.fleet.azure.com"},
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{"system:masters"},
			},
			wantResponse: admission.Allowed(fmt.Sprintf(crdAllowedFormat, "test-user", []string{mastersGroup}, types.NamespacedName{Name: "memberclusters.fleet.azure.com"})),
		},
		"allow white listed user to modify fleet CRD": {
			group:          "fleet.azure.com",
			namespacedName: types.NamespacedName{Name: "memberclusters.fleet.azure.com"},
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{"test-group"},
			},
			whiteListedUsers: []string{"test-user"},
			wantResponse:     admission.Allowed(fmt.Sprintf(crdAllowedFormat, "test-user", []string{"test-group"}, types.NamespacedName{Name: "memberclusters.fleet.azure.com"})),
		},
		"deny user not in system:masters group to modify fleet CRD": {
			group:          "fleet.azure.com",
			namespacedName: types.NamespacedName{Name: "memberclusters.fleet.azure.com"},
			userInfo: v1.UserInfo{
				Username: "test-user",
				Groups:   []string{"test-group"},
			},
			wantResponse: admission.Denied(fmt.Sprintf(crdDeniedFormat, "test-user", []string{"test-group"}, types.NamespacedName{Name: "memberclusters.fleet.azure.com"})),
		},
	}

	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			gotResult := ValidateUserForFleetCRD(testCase.group, testCase.namespacedName, testCase.whiteListedUsers, testCase.userInfo)
			assert.Equal(t, testCase.wantResponse, gotResult, utils.TestCaseMsg, testName)
		})
	}
}
