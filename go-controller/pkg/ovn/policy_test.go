package ovn

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilnet "k8s.io/utils/net"
)

// Legacy const, should only be used in sync and tests
const (
	// arpAllowPolicySuffix is the suffix used when creating default ACLs for a namespace
	arpAllowPolicySuffix = "ARPallowPolicy"
)

func getFakeController(controllerName string) *DefaultNetworkController {
	controller := &DefaultNetworkController{
		BaseNetworkController: BaseNetworkController{
			controllerName: controllerName,
			NetInfo:        &util.DefaultNetInfo{},
			NetConfInfo:    &util.DefaultNetConfInfo{},
		},
	}
	return controller
}

func newNetworkPolicy(name, namespace string, podSelector metav1.LabelSelector, ingress []knet.NetworkPolicyIngressRule,
	egress []knet.NetworkPolicyEgressRule, policyTypes ...knet.PolicyType) *knet.NetworkPolicy {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			UID:       apimachinerytypes.UID(namespace),
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"name": name,
			},
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: podSelector,
			PolicyTypes: policyTypes,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
	if policyTypes == nil {
		if len(ingress) > 0 {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, knet.PolicyTypeIngress)
		}
		if len(egress) > 0 {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, knet.PolicyTypeEgress)
		}
	}
	return policy
}

func getFakeBaseController(netInfo util.NetInfo, netConfInfo util.NetConfInfo) *BaseNetworkController {
	return &BaseNetworkController{
		controllerName: netInfo.GetNetworkName() + "-network-controller",
		NetInfo:        netInfo,
		NetConfInfo:    netConfInfo,
	}
}

func getDefaultDenyData(networkPolicy *knet.NetworkPolicy, ports []string,
	denyLogSeverity nbdb.ACLSeverity, stale bool, netInfo util.NetInfo) []libovsdb.TestData {
	fakeController := getFakeBaseController(netInfo, nil)
	egressPGName := fakeController.defaultDenyPortGroupName(networkPolicy.Namespace, egressDefaultDenySuffix)
	policyTypeIngress, policyTypeEgress := getPolicyType(networkPolicy)
	shouldBeLogged := denyLogSeverity != ""
	aclIDs := fakeController.getDefaultDenyPolicyACLIDs(networkPolicy.Namespace, aclEgress, defaultDenyACL)
	egressDenyACL := libovsdbops.BuildACL(
		getACLName(aclIDs),
		nbdb.ACLDirectionFromLport,
		types.DefaultDenyPriority,
		"inport == @"+egressPGName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denyLogSeverity,
		shouldBeLogged,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
	)
	egressDenyACL.UUID = aclIDs.String() + "-UUID"

	aclIDs = fakeController.getDefaultDenyPolicyACLIDs(networkPolicy.Namespace, aclEgress, arpAllowACL)
	egressAllowACL := libovsdbops.BuildACL(
		getACLName(aclIDs),
		nbdb.ACLDirectionFromLport,
		types.DefaultAllowPriority,
		"inport == @"+egressPGName+" && "+arpAllowPolicyMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
	)
	egressAllowACL.UUID = aclIDs.String() + "-UUID"

	ingressPGName := fakeController.defaultDenyPortGroupName(networkPolicy.Namespace, ingressDefaultDenySuffix)
	aclIDs = fakeController.getDefaultDenyPolicyACLIDs(networkPolicy.Namespace, aclIngress, defaultDenyACL)
	ingressDenyACL := libovsdbops.BuildACL(
		getACLName(aclIDs),
		nbdb.ACLDirectionToLport,
		types.DefaultDenyPriority,
		"outport == @"+ingressPGName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denyLogSeverity,
		shouldBeLogged,
		aclIDs.GetExternalIDs(),
		nil,
	)
	ingressDenyACL.UUID = aclIDs.String() + "-UUID"

	aclIDs = fakeController.getDefaultDenyPolicyACLIDs(networkPolicy.Namespace, aclIngress, arpAllowACL)
	ingressAllowACL := libovsdbops.BuildACL(
		getACLName(aclIDs),
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		"outport == @"+ingressPGName+" && "+arpAllowPolicyMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
	)
	ingressAllowACL.UUID = aclIDs.String() + "-UUID"

	if stale {
		getStaleDefaultACL([]*nbdb.ACL{egressDenyACL, egressAllowACL}, networkPolicy.Namespace, networkPolicy.Name)
		getStaleDefaultACL([]*nbdb.ACL{ingressDenyACL, ingressAllowACL}, networkPolicy.Namespace, networkPolicy.Name)
	}

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range ports {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	var egressDenyPorts []*nbdb.LogicalSwitchPort
	if policyTypeEgress {
		egressDenyPorts = lsps
	}
	egressDenyPG := fakeController.buildPortGroup(
		egressPGName,
		egressPGName,
		egressDenyPorts,
		[]*nbdb.ACL{egressDenyACL, egressAllowACL},
	)
	egressDenyPG.UUID = egressDenyPG.Name + "-UUID"

	var ingressDenyPorts []*nbdb.LogicalSwitchPort
	if policyTypeIngress {
		ingressDenyPorts = lsps
	}
	ingressDenyPG := fakeController.buildPortGroup(
		ingressPGName,
		ingressPGName,
		ingressDenyPorts,
		[]*nbdb.ACL{ingressDenyACL, ingressAllowACL},
	)
	ingressDenyPG.UUID = ingressDenyPG.Name + "-UUID"

	return []libovsdb.TestData{
		egressDenyACL,
		egressAllowACL,
		ingressDenyACL,
		ingressAllowACL,
		egressDenyPG,
		ingressDenyPG,
	}
}

func getStaleARPAllowACLName(ns string) string {
	return joinACLName(ns, arpAllowPolicySuffix)
}

func getStaleDefaultACL(acls []*nbdb.ACL, namespace, policyName string) []*nbdb.ACL {
	for _, acl := range acls {
		var staleName string
		switch acl.ExternalIDs[libovsdbops.TypeKey.String()] {
		case string(defaultDenyACL):
			staleName = namespace + "_" + policyName
		case string(arpAllowACL):
			staleName = getStaleARPAllowACLName(namespace)
		}
		acl.Name = &staleName
		acl.Options = nil
		acl.Direction = nbdb.ACLDirectionToLport
	}
	return acls
}

func getMultinetNsAddrSetHashNames(ns, controllerName string) (string, string) {
	return addressset.GetHashNamesForAS(getNamespaceAddrSetDbIDs(ns, controllerName))
}

func getGressACLs(i int, namespace, policyName string, peerNamespaces []string, tcpPeerPorts []int32,
	peers []knet.NetworkPolicyPeer, logSeverity nbdb.ACLSeverity, policyType knet.PolicyType, stale,
	statlessNetPol bool, netInfo util.NetInfo) []*nbdb.ACL {
	fakeController := getFakeBaseController(netInfo, nil)
	pgName, _ := fakeController.getNetworkPolicyPGName(namespace, policyName)
	controllerName := netInfo.GetNetworkName() + "-network-controller"
	shouldBeLogged := logSeverity != ""
	var options map[string]string
	var direction string
	var portDir string
	var ipDir string
	acls := []*nbdb.ACL{}
	if policyType == knet.PolicyTypeEgress {
		options = map[string]string{
			"apply-after-lb": "true",
		}
		direction = nbdb.ACLDirectionFromLport
		portDir = "inport"
		ipDir = "dst"
	} else {
		direction = nbdb.ACLDirectionToLport
		portDir = "outport"
		ipDir = "src"
	}
	hashedASNames := []string{}
	for _, nsName := range peerNamespaces {
		hashedASName, _ := getMultinetNsAddrSetHashNames(nsName, controllerName)
		hashedASNames = append(hashedASNames, hashedASName)
	}
	ipBlock := ""
	for _, peer := range peers {
		if peer.PodSelector != nil && len(peerNamespaces) == 0 {
			peerIndex := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), controllerName)
			asv4, _ := addressset.GetHashNamesForAS(peerIndex)
			hashedASNames = append(hashedASNames, asv4)
		}
		if peer.IPBlock != nil {
			ipBlock = peer.IPBlock.CIDR
		}
	}
	gp := gressPolicy{
		policyNamespace: namespace,
		policyName:      policyName,
		policyType:      policyType,
		idx:             i,
		controllerName:  controllerName,
	}
	if len(hashedASNames) > 0 {
		gressAsMatch := asMatch(hashedASNames)
		match := fmt.Sprintf("ip4.%s == {%s} && %s == @%s", ipDir, gressAsMatch, portDir, pgName)
		action := nbdb.ACLActionAllowRelated
		if statlessNetPol {
			action = nbdb.ACLActionAllowStateless
		}
		dbIDs := gp.getNetpolACLDbIDs(emptyIdx, emptyIdx)
		acl := libovsdbops.BuildACL(
			getACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			match,
			action,
			types.OvnACLLoggingMeter,
			logSeverity,
			shouldBeLogged,
			dbIDs.GetExternalIDs(),
			options,
		)
		acl.UUID = dbIDs.String() + "-UUID"
		acls = append(acls, acl)
	}
	if ipBlock != "" {
		match := fmt.Sprintf("ip4.%s == %s && %s == @%s", ipDir, ipBlock, portDir, pgName)
		dbIDs := gp.getNetpolACLDbIDs(emptyIdx, 0)
		acl := libovsdbops.BuildACL(
			getACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			match,
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			logSeverity,
			shouldBeLogged,
			dbIDs.GetExternalIDs(),
			options,
		)
		acl.UUID = dbIDs.String() + "-UUID"
		acls = append(acls, acl)
	}
	for portPolicyIdx, v := range tcpPeerPorts {
		dbIDs := gp.getNetpolACLDbIDs(portPolicyIdx, emptyIdx)
		acl := libovsdbops.BuildACL(
			getACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			fmt.Sprintf("ip4 && tcp && tcp.dst==%d && %s == @%s", v, portDir, pgName),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			logSeverity,
			shouldBeLogged,
			dbIDs.GetExternalIDs(),
			options,
		)
		acl.UUID = dbIDs.String() + "-UUID"
		acls = append(acls, acl)
	}
	if stale {
		return getStalePolicyACL(acls, namespace, policyName)
	}
	return acls
}

// getStalePolicyACL updated ACL to the most ancient version, which includes
// - old name
// - no options for Egress ACLs
// - wrong direction for egress ACLs
// - non-nil Severity when Log is false
func getStalePolicyACL(acls []*nbdb.ACL, policyNamespace, policyName string) []*nbdb.ACL {
	for i, acl := range acls {
		staleName := policyNamespace + "_" + policyName + "_" + strconv.Itoa(i)
		acl.Name = &staleName
		acl.Options = nil
		acl.Direction = nbdb.ACLDirectionToLport
		sev := nbdb.ACLSeverityInfo
		acl.Severity = &sev
	}
	return acls
}

func getPolicyData(networkPolicy *knet.NetworkPolicy, localPortUUIDs []string, peerNamespaces []string,
	tcpPeerPorts []int32, allowLogSeverity nbdb.ACLSeverity, stale, statlessNetPol bool, netInfo util.NetInfo) []libovsdbtest.TestData {
	acls := []*nbdb.ACL{}

	for i, ingress := range networkPolicy.Spec.Ingress {
		acls = append(acls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name,
			peerNamespaces, tcpPeerPorts, ingress.From, allowLogSeverity, knet.PolicyTypeIngress, stale, statlessNetPol, netInfo)...)
	}
	for i, egress := range networkPolicy.Spec.Egress {
		acls = append(acls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name,
			peerNamespaces, tcpPeerPorts, egress.To, allowLogSeverity, knet.PolicyTypeEgress, stale, statlessNetPol, netInfo)...)
	}

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range localPortUUIDs {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	fakeController := getFakeBaseController(netInfo, nil)
	pgName, readableName := fakeController.getNetworkPolicyPGName(networkPolicy.Namespace, networkPolicy.Name)
	pg := fakeController.buildPortGroup(
		pgName,
		readableName,
		lsps,
		acls,
	)
	pg.UUID = pg.Name + "-UUID"

	data := []libovsdb.TestData{}
	for _, acl := range acls {
		data = append(data, acl)
	}
	data = append(data, pg)
	return data
}

func getHairpinningACLsV4AndPortGroup() []libovsdbtest.TestData {
	clusterPortGroup := newClusterPortGroup()
	fakeController := getFakeController(DefaultNetworkControllerName)
	egressIDs := fakeController.getNetpolDefaultACLDbIDs("Egress")
	egressACL := libovsdbops.BuildACL(
		"",
		nbdb.ACLDirectionFromLport,
		types.DefaultAllowPriority,
		fmt.Sprintf("%s.src == %s", "ip4", types.V4OVNServiceHairpinMasqueradeIP),
		nbdb.ACLActionAllowRelated,
		types.OvnACLLoggingMeter,
		"",
		false,
		egressIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
	)
	egressACL.UUID = "hairpinning-egress-UUID"
	ingressIDs := fakeController.getNetpolDefaultACLDbIDs("Ingress")
	ingressACL := libovsdbops.BuildACL(
		"",
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		fmt.Sprintf("%s.src == %s", "ip4", types.V4OVNServiceHairpinMasqueradeIP),
		nbdb.ACLActionAllowRelated,
		types.OvnACLLoggingMeter,
		"",
		false,
		ingressIDs.GetExternalIDs(),
		nil,
	)
	ingressACL.UUID = "hairpinning-ingress-UUID"
	clusterPortGroup.ACLs = []string{egressACL.UUID, ingressACL.UUID}
	return []libovsdb.TestData{egressACL, ingressACL, clusterPortGroup}
}

func eventuallyExpectNoAddressSets(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace string) {
	dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
	fakeOvn.asf.EventuallyExpectNoAddressSet(dbIDs)
}

func eventuallyExpectAddressSetsWithIP(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace, ip string) {
	if peer.PodSelector != nil {
		dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectAddressSetWithIPs(dbIDs, []string{ip})
	}
}

func eventuallyExpectEmptyAddressSetsExist(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace string) {
	if peer.PodSelector != nil {
		dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(dbIDs)
	}
}

func getMatchLabelsNetworkPolicy(policyName, netpolNamespace, peerNamespace, peerPodName string, ingress, egress bool) *knet.NetworkPolicy {
	netPolPeer := knet.NetworkPolicyPeer{}
	if peerPodName != "" {
		netPolPeer.PodSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": peerPodName,
			},
		}
	}
	if peerNamespace != "" {
		netPolPeer.NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": peerNamespace,
			},
		}
	}
	var ingressRules []knet.NetworkPolicyIngressRule
	if ingress {
		ingressRules = []knet.NetworkPolicyIngressRule{
			{
				From: []knet.NetworkPolicyPeer{netPolPeer},
			},
		}
	}
	var egressRules []knet.NetworkPolicyEgressRule
	if egress {
		egressRules = []knet.NetworkPolicyEgressRule{
			{
				To: []knet.NetworkPolicyPeer{netPolPeer},
			},
		}
	}
	return newNetworkPolicy(policyName, netpolNamespace, metav1.LabelSelector{}, ingressRules, egressRules)
}

func getPortNetworkPolicy(policyName, namespace, labelName, labelVal string, tcpPort int32) *knet.NetworkPolicy {
	tcpProtocol := v1.ProtocolTCP
	return newNetworkPolicy(policyName, namespace,
		metav1.LabelSelector{
			MatchLabels: map[string]string{
				labelName: labelVal,
			},
		},
		[]knet.NetworkPolicyIngressRule{{
			Ports: []knet.NetworkPolicyPort{{
				Port:     &intstr.IntOrString{IntVal: tcpPort},
				Protocol: &tcpProtocol,
			}},
		}},
		[]knet.NetworkPolicyEgressRule{{
			Ports: []knet.NetworkPolicyPort{{
				Port:     &intstr.IntOrString{IntVal: tcpPort},
				Protocol: &tcpProtocol,
			}},
		}},
	)
}

func getTestPod(namespace, nodeName string) testPod {
	return newTPod(
		nodeName,
		"10.128.1.0/24",
		"10.128.1.2",
		"10.128.1.1",
		"myPod",
		"10.128.1.3",
		"0a:58:0a:80:01:03",
		namespace,
	)
}

var _ = ginkgo.Describe("OVN NetworkPolicy Operations", func() {
	const (
		namespaceName1        = "namespace1"
		namespaceName2        = "namespace2"
		netPolicyName1        = "networkpolicy1"
		netPolicyName2        = "networkpolicy2"
		nodeName              = "node1"
		labelName      string = "pod-name"
		labelVal       string = "server"
		portNum        int32  = 81
	)
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdb.TestSetup

		gomegaFormatMaxLength int
		logicalSwitch         *nbdb.LogicalSwitch
		clusterPortGroup      *nbdb.PortGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)

		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
		logicalSwitch = &nbdb.LogicalSwitch{
			Name: nodeName,
			UUID: nodeName + "_UUID",
		}
		clusterPortGroup = newClusterPortGroup()
		initialData := getHairpinningACLsV4AndPortGroup()
		initialData = append(initialData, logicalSwitch)
		initialDB = libovsdb.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	startOvn := func(dbSetup libovsdb.TestSetup, namespaces []v1.Namespace, networkPolicies []knet.NetworkPolicy,
		pods []testPod, podLabels map[string]string) {
		var podsList []v1.Pod
		for _, testPod := range pods {
			knetPod := newPod(testPod.namespace, testPod.podName, testPod.nodeName, testPod.podIP)
			if len(podLabels) > 0 {
				knetPod.Labels = podLabels
			}
			podsList = append(podsList, *knetPod)
		}
		fakeOvn.startWithDBSetup(dbSetup,
			&v1.NamespaceList{
				Items: namespaces,
			},
			&v1.PodList{
				Items: podsList,
			},
			&knet.NetworkPolicyList{
				Items: networkPolicies,
			},
		)
		for _, testPod := range pods {
			testPod.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, nodeName))
		}
		var err error
		if namespaces != nil {
			err = fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		if pods != nil {
			err = fakeOvn.controller.WatchPods()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		err = fakeOvn.controller.WatchNetworkPolicy()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	getUpdatedInitialDB := func(tPods []testPod) []libovsdbtest.TestData {
		updatedSwitchAndPods := getExpectedDataPodsAndSwitches(tPods, []string{nodeName})
		return append(getHairpinningACLsV4AndPortGroup(), updatedSwitchAndPods...)
	}

	ginkgo.Context("on startup", func() {
		ginkgo.It("creates default hairpinning ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				clusterPortGroup = newClusterPortGroup()
				initialDB = libovsdb.TestSetup{
					NBData: []libovsdb.TestData{
						logicalSwitch,
						clusterPortGroup,
					},
				}
				startOvn(initialDB, nil, nil, nil, nil)

				hairpinningACLs := getHairpinningACLsV4AndPortGroup()
				expectedData := []libovsdb.TestData{logicalSwitch}
				expectedData = append(expectedData, hairpinningACLs...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deletes stale port groups", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				// network policy with peer selector
				networkPolicy1 := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", "podName", true, true)
				// network policy with ipBlock (won't have any address sets)
				networkPolicy2 := newNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{{
							IPBlock: &knet.IPBlock{
								CIDR: "1.1.1.0/24",
							}},
						}},
					}, nil)

				initialData := initialDB.NBData
				afterCleanup := initialDB.NBData
				policy1Db := getPolicyData(networkPolicy1, nil, []string{}, nil, "", false, false, &util.DefaultNetInfo{})
				initialData = append(initialData, policy1Db...)
				// since acls are not garbage-collected, only port group will be deleted
				afterCleanup = append(afterCleanup, policy1Db[:len(policy1Db)-1]...)

				policy2Db := getPolicyData(networkPolicy2, nil, []string{}, nil, "", false, false, &util.DefaultNetInfo{})
				initialData = append(initialData, policy2Db...)
				// since acls are not garbage-collected, only port group will be deleted
				afterCleanup = append(afterCleanup, policy2Db[:len(policy2Db)-1]...)

				defaultDenyDb := getDefaultDenyData(networkPolicy1, nil, "", false, &util.DefaultNetInfo{})
				initialData = append(initialData, defaultDenyDb...)
				// since acls are not garbage-collected, only port groups will be deleted
				afterCleanup = append(afterCleanup, defaultDenyDb[:len(defaultDenyDb)-2]...)

				// start ovn with no objects, expect stale port db entries to be cleaned up
				startOvn(libovsdb.TestSetup{NBData: initialData}, nil, nil, nil, nil)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(afterCleanup))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing networkPolicy with empty db", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData := initialDB.NBData
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing networkPolicy updating stale ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				// start with stale ACLs
				gressPolicyInitialData := getPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", true, false, &util.DefaultNetInfo{})
				defaultDenyInitialData := getDefaultDenyData(networkPolicy, nil, "", true, &util.DefaultNetInfo{})
				initialData := initialDB.NBData
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)
				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// make sure stale ACLs were updated
				expectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, initialDB.NBData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an existing networkPolicy updating stale ACLs with long names", func() {
			app.Action = func(ctx *cli.Context) error {
				longNamespaceName63 := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // longest allowed namespace name
				namespace1 := *newNamespace(longNamespaceName63)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				// start with stale ACLs
				gressPolicyInitialData := getPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", true, false, &util.DefaultNetInfo{})
				defaultDenyInitialData := getDefaultDenyData(networkPolicy, nil, "", true, &util.DefaultNetInfo{})
				initialData := initialDB.NBData
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)
				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(longNamespaceName63)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// make sure stale ACLs were updated
				expectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, initialDB.NBData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an existing networkPolicy updating stale address sets", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", false, true)
				// start with stale ACLs
				staleAddrSetIDs := getStaleNetpolAddrSetDbIDs(networkPolicy.Namespace, networkPolicy.Name,
					"egress", "0", DefaultNetworkControllerName)
				localASName, _ := addressset.GetHashNamesForAS(staleAddrSetIDs)
				peerASName, _ := getNsAddrSetHashNames(namespace2.Name)
				fakeController := getFakeController(DefaultNetworkControllerName)
				pgName, _ := fakeController.getNetworkPolicyPGName(networkPolicy.Namespace, networkPolicy.Name)
				initialData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false, false, &util.DefaultNetInfo{})
				staleACL := initialData[0].(*nbdb.ACL)
				staleACL.Match = fmt.Sprintf("ip4.dst == {$%s, $%s} && inport == @%s", localASName, peerASName, pgName)

				defaultDenyInitialData := getDefaultDenyData(networkPolicy, nil, "", true, &util.DefaultNetInfo{})
				initialData = append(initialData, defaultDenyInitialData...)
				initialData = append(initialData, initialDB.NBData...)
				_, err := fakeOvn.asf.NewAddressSet(staleAddrSetIDs, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				// make sure stale ACLs were updated
				expectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, initialDB.NBData...)
				fakeOvn.asf.ExpectEmptyAddressSet(staleAddrSetIDs)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an ingress networkPolicy updating an existing ACL", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)

				// network policy without peer namespace
				gressPolicyInitialData := getPolicyData(networkPolicy, nil, []string{}, nil,
					"", false, false, &util.DefaultNetInfo{})
				defaultDenyInitialData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				initialData := initialDB.NBData
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)
				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check peer namespace was added
				expectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false, false, &util.DefaultNetInfo{})
				expectedData = append(expectedData, defaultDenyInitialData...)
				expectedData = append(expectedData, initialDB.NBData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing networkPolicy with a pod selector in its own namespace from empty db", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				// network policy with peer pod selector
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing networkPolicy with a pod and namespace selector in another namespace from empty db", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := getTestPod(namespace2.Name, nodeName)
				// network policy with peer pod and namespace selector
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{}, nil,
					"", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("during execution", func() {
		table.DescribeTable("correctly uses namespace and shared peer selector address sets",
			func(peer knet.NetworkPolicyPeer, peerNamespaces []string) {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				netpol := newNetworkPolicy("netpolName", namespace1.Name, metav1.LabelSelector{}, []knet.NetworkPolicyIngressRule{
					{
						From: []knet.NetworkPolicyPeer{peer},
					},
				}, nil)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*netpol}, nil, nil)
				expectedData := initialDB.NBData
				gressPolicyExpectedData := getPolicyData(netpol, nil,
					peerNamespaces, nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(netpol, nil, "", false, &util.DefaultNetInfo{})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
			},
			table.Entry("for empty pod selector => use netpol namespace address set",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{},
				}, []string{namespaceName1}),
			table.Entry("namespace selector with empty pod selector => use a set of selected namespace address sets",
				knet.NetworkPolicyPeer{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": namespaceName2,
						},
					},
				}, []string{namespaceName2}),
			table.Entry("empty namespace and pod selector => use all pods shared address set",
				knet.NetworkPolicyPeer{
					PodSelector:       &metav1.LabelSelector{},
					NamespaceSelector: &metav1.LabelSelector{},
				}, nil),
			table.Entry("pod selector with nil namespace => use static namespace+pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
				}, nil),
			table.Entry("pod selector with namespace selector => use namespace selector+pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": namespaceName2,
						},
					},
				}, nil),
			table.Entry("pod selector with empty namespace selector => use global pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
					NamespaceSelector: &metav1.LabelSelector{},
				}, nil),
		)

		ginkgo.It("correctly creates and deletes a networkpolicy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1)

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false, false, &util.DefaultNetInfo{})
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPolicy2...))

				// Delete the second network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).
					Delete(context.TODO(), networkPolicy2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// TODO: test server does not garbage collect ACLs, so we just expect policy2 portgroup to be removed
				expectedDataWithoutPolicy2 := append(expectedData, gressPolicy2ExpectedData[:len(gressPolicy2ExpectedData)-1]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPolicy2...))

				// Delete the first network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				expectedData = append(expectedData, gressPolicy2ExpectedData[:len(gressPolicy2ExpectedData)-1]...)
				expectedData = append(expectedData, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries creating a network policy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Bringing down NBDB")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1)
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(types.OVSDBTimeout + time.Second)
				// check to see if the retry cache has an entry for this policy
				key, err := retry.GetResourceKey(networkPolicy2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryNetworkPolicies)

				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()

				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false, false, &util.DefaultNetInfo{})
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPolicy2...))
				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries recreating a network policy with the same name", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Bringing down NBDB")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				ginkgo.By("Delete the first network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(types.OVSDBTimeout + time.Second)
				// create second networkpolicy with the same name, but different tcp port
				networkPolicy2 := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum+1)
				// check retry entry for this policy
				key, err := retry.GetResourceKey(networkPolicy2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("retry entry: old obj should not be nil, new obj should be nil")
				retry.CheckRetryObjectMultipleFieldsEventually(
					key,
					fakeOvn.controller.retryNetworkPolicies,
					gomega.Not(gomega.BeNil()), // oldObj should not be nil
					gomega.BeNil(),             // newObj should be nil
				)
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				ginkgo.By("Create a new network policy with same name")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData2 := getDefaultDenyData(networkPolicy2, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy2ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData2...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				// check the cache no longer has the entry
				key, err = retry.GetResourceKey(networkPolicy)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted namespace referenced by a networkpolicy with a local running pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)
				// create networkPolicy, check db
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					[]string{namespace2.Name}, nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete peer namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				// acls will be deleted, since no namespaces are selected at this point.
				// since test server doesn't garbage-collect de-referenced acls, they will stay in the db
				// gressPolicyExpectedData[2] is the policy port group
				gressPolicyExpectedData[2].(*nbdb.PortGroup).ACLs = nil
				//gressPolicyExpectedData = getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
				//	nil, "", false)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted namespace referenced by a networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					nil, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData := initialDB.NBData
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// delete namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = initialDB.NBData
				// acls will be deleted, since no namespaces are selected at this point.
				// since test server doesn't garbage-collect de-referenced acls, they will stay in the db
				// gressPolicyExpectedData[2] is the policy port group
				gressPolicyExpectedData[2].(*nbdb.PortGroup).ACLs = nil
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				gressPolicyExpectedData = getPolicyData(networkPolicy, nil, []string{}, nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData = getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData = initialDB.NBData
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in another namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := getTestPod(namespace2.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					nPodTest.namespace, nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{},
					nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData1 := append(expectedData, getUpdatedInitialDB([]testPod{nPodTest})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData1...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData2 := append(expectedData, getUpdatedInitialDB([]testPod{})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData2...))

				// After deleting the pod all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an updated namespace label", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				nPodTest := getTestPod(namespace2.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					nPodTest.namespace, nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{}, nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Update namespace labels
				namespace2.ObjectMeta.Labels = map[string]string{"labels": "test"}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// After updating the namespace all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				// db data should stay the same
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy.Spec.Ingress[0].From[0], networkPolicy.Namespace)

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData[:len(gressPolicyExpectedData)-1]...)
				expectedData = append(expectedData, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("retries a deleted network policy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeTrue())
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(types.OVSDBTimeout + time.Second)

				// check to see if the retry cache has an entry for this policy
				key, err := retry.GetResourceKey(networkPolicy)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryNetworkPolicies)
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()

				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy.Spec.Ingress[0].From[0], networkPolicy.Namespace)

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData[:len(gressPolicyExpectedData)-1]...)
				expectedData = append(expectedData, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.controller.nbClient).Should(libovsdb.HaveData(expectedData))

				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a policy isolating for ingress and processing egress rules", func() {
			// Even though a policy might isolate for ingress only, we need to
			// process the egress rules in case an additional policy isolating
			// for egress is added in the future.
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy(netPolicyName1, namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					nil,
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					knet.PolicyTypeIngress,
				)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Creating a network policy that isolates a pod for ingress with egress rules")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil,
					[]int32{portNum}, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				expectedData = append(expectedData, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a policy isolating for egress and processing ingress rules", func() {
			// Even though a policy might isolate for egress only, we need to
			// process the ingress rules in case an additional policy isolating
			// for ingress is added in the future.
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy(netPolicyName1, namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					nil,
					knet.PolicyTypeEgress,
				)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Creating a network policy that isolates a pod for egress with ingress rules")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", false, false, &util.DefaultNetInfo{})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				expectedData = append(expectedData, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("can reconcile network policy with long name", func() {
			app.Action = func(ctx *cli.Context) error {
				// this problem can be reproduced by starting ovn with existing db rows for network policy
				// from namespace with long name, but WatchNetworkPolicy doesn't return error on initial netpol add,
				// it just puts network policy to retry loop.
				// To check the error message directly, we can explicitly add network policy, then
				// delete NetworkPolicy's resources to pretend controller doesn't know about it.
				// Then on the next addNetworkPolicy call the result should be the same as on restart.
				// Before ACLs were updated to have new DbIDs, defaultDeny acls (arp and default deny)
				// were equivalent, since their names were cropped and only contained namespace name,
				// and externalIDs only had defaultDenyPolicyTypeACLExtIdKey: Egress/Ingress.
				// Now ExternalIDs will always be different, and ACLs won't be equivalent.
				longNameSpaceName := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // create with 63 characters
				longNamespace := *newNamespace(longNameSpaceName)
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, longNamespace.Name, labelName, labelVal, portNum)

				startOvn(initialDB, []v1.Namespace{longNamespace}, []knet.NetworkPolicy{*networkPolicy1},
					nil, map[string]string{labelName: labelVal})

				ginkgo.By("Simulate the initial re-add of all network policies during upgrade and ensure we are stable")
				// pretend controller didn't see this netpol object, all related db rows are still present
				fakeOvn.controller.networkPolicies.Delete(getPolicyKey(networkPolicy1))
				fakeOvn.controller.sharedNetpolPortGroups.Delete(networkPolicy1.Namespace)
				err := fakeOvn.controller.addNetworkPolicy(networkPolicy1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("ACL logging for network policies", func() {

		var originalNamespace v1.Namespace

		updateNamespaceACLLogSeverity := func(namespaceToUpdate *v1.Namespace, desiredDenyLogLevel string, desiredAllowLogLevel string) error {
			ginkgo.By("updating the namespace's ACL logging severity")
			updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, desiredDenyLogLevel, desiredAllowLogLevel)
			namespaceToUpdate.Annotations[util.AclLoggingAnnotation] = updatedLogSeverity

			_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceToUpdate, metav1.UpdateOptions{})
			return err
		}

		ginkgo.BeforeEach(func() {
			originalACLLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice)
			originalNamespace = *newNamespace(namespaceName1)
			originalNamespace.Annotations = map[string]string{util.AclLoggingAnnotation: originalACLLogSeverity}
		})

		table.DescribeTable("ACL logging for network policies reacts to severity updates", func(networkPolicies ...*knet.NetworkPolicy) {
			ginkgo.By("Provisioning the system with an initial empty policy, we know deterministically the names of the default deny ACLs")
			initialDenyAllPolicy := newNetworkPolicy("emptyPol", namespaceName1, metav1.LabelSelector{}, nil, nil)
			// originalACLLogSeverity.Deny == nbdb.ACLSeverityAlert
			initialExpectedData := getDefaultDenyData(initialDenyAllPolicy, nil, nbdb.ACLSeverityAlert, false, &util.DefaultNetInfo{})
			initialExpectedData = append(initialExpectedData,
				// no gress policies defined, return only port group
				getPolicyData(initialDenyAllPolicy, nil, []string{}, nil, nbdb.ACLSeverityNotice, false, false, &util.DefaultNetInfo{})...)
			initialExpectedData = append(initialExpectedData, initialDB.NBData...)

			app.Action = func(ctx *cli.Context) error {
				startOvn(initialDB, []v1.Namespace{originalNamespace}, []knet.NetworkPolicy{*initialDenyAllPolicy},
					nil, nil)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialExpectedData...))

				var createsPoliciesData []libovsdb.TestData
				// create network policies for given Entry
				for i := range networkPolicies {
					ginkgo.By("Creating new network policy")
					_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicies[i].GetNamespace()).
						Create(context.TODO(), networkPolicies[i], metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					createsPoliciesData = append(createsPoliciesData,
						// originalACLLogSeverity.Allow == nbdb.ACLSeverityNotice
						getPolicyData(networkPolicies[i], nil, []string{}, nil, nbdb.ACLSeverityNotice, false, false, &util.DefaultNetInfo{})...)
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(initialExpectedData, createsPoliciesData...)))

				// update namespace log severity
				updatedLogSeverity := nbdb.ACLSeverityDebug
				gomega.Expect(
					updateNamespaceACLLogSeverity(&originalNamespace, updatedLogSeverity, updatedLogSeverity)).To(gomega.Succeed(),
					"should have managed to update the ACL logging severity within the namespace")

				// update expected data log severity
				expectedData := getDefaultDenyData(initialDenyAllPolicy, nil, updatedLogSeverity, false, &util.DefaultNetInfo{})
				expectedData = append(expectedData,
					// no gress policies defined, return only port group
					getPolicyData(initialDenyAllPolicy, nil, []string{}, nil, updatedLogSeverity, false, false, &util.DefaultNetInfo{})...)
				for i := range networkPolicies {
					expectedData = append(expectedData,
						getPolicyData(networkPolicies[i], nil, []string{}, nil, updatedLogSeverity, false, false, &util.DefaultNetInfo{})...)
				}
				expectedData = append(expectedData, initialDB.NBData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
			table.Entry("when the namespace features a network policy with a single rule",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false)),
			table.Entry("when the namespace features *multiple* network policies with a single rule",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false),
				getMatchLabelsNetworkPolicy(netPolicyName2, namespaceName1, namespaceName2, "", false, true)),
			table.Entry("when the namespace features a network policy with *multiple* rules",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "tiny-winy-pod", true, false)))

		ginkgo.It("policies created after namespace logging level updates inherit updated logging level", func() {
			app.Action = func(ctx *cli.Context) error {
				startOvn(initialDB, []v1.Namespace{originalNamespace}, nil, nil, nil)
				desiredLogSeverity := nbdb.ACLSeverityDebug
				// update namespace log severity
				gomega.Expect(
					updateNamespaceACLLogSeverity(&originalNamespace, desiredLogSeverity, desiredLogSeverity)).To(gomega.Succeed(),
					"should have managed to update the ACL logging severity within the namespace")

				newPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false)
				ginkgo.By("Creating new network policy")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(namespaceName1).
					Create(context.TODO(), newPolicy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should have managed to create a new network policy")

				expectedData := initialDB.NBData
				expectedData = append(expectedData, getPolicyData(newPolicy, nil, []string{}, nil, desiredLogSeverity, false, false, &util.DefaultNetInfo{})...)
				expectedData = append(expectedData, getDefaultDenyData(newPolicy, nil, desiredLogSeverity, false, &util.DefaultNetInfo{})...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates stateless OVN ACLs based off of the annotation", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				networkPolicy.Annotations = map[string]string{
					ovnStatelessNetPolAnnotationName: "true",
				}
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				statelessPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false, true, &util.DefaultNetInfo{})

				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false, &util.DefaultNetInfo{})
				expectedData = append(expectedData, statelessPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

	})
})

func asMatch(hashedAddressSets []string) string {
	sort.Strings(hashedAddressSets)
	var match string
	for i, n := range hashedAddressSets {
		if i > 0 {
			match += ", "
		}
		match += fmt.Sprintf("$%s", n)
	}
	return match
}

func getAllowFromNodeExpectedACL(nodeName, mgmtIP string, logicalSwitch *nbdb.LogicalSwitch, controllerName string) *nbdb.ACL {
	var ipFamily = "ip4"
	if utilnet.IsIPv6(ovntest.MustParseIP(mgmtIP)) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtIP)

	dbIDs := getAllowFromNodeACLDbIDs(nodeName, mgmtIP, controllerName)
	nodeACL := libovsdbops.BuildACL(
		getACLName(dbIDs),
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		match,
		nbdb.ACLActionAllowRelated,
		types.OvnACLLoggingMeter,
		"",
		false,
		dbIDs.GetExternalIDs(),
		nil)
	nodeACL.UUID = dbIDs.String() + "-UUID"
	if logicalSwitch != nil {
		logicalSwitch.ACLs = []string{nodeACL.UUID}
	}
	return nodeACL
}

func getAllowFromNodeStaleACL(nodeName, mgmtIP string, logicalSwitch *nbdb.LogicalSwitch, controllerName string) *nbdb.ACL {
	acl := getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName)
	newName := ""
	acl.Name = &newName

	return acl
}

// here only low-level operation are tested (directly calling updateStaleNetpolNodeACLs)
var _ = ginkgo.Describe("OVN AllowFromNode ACL low-level operations", func() {
	var (
		nbCleanup     *libovsdbtest.Cleanup
		logicalSwitch *nbdb.LogicalSwitch
	)

	const (
		nodeName       = "node1"
		ipv4MgmtIP     = "192.168.10.10"
		ipv6MgmtIP     = "fd01::1234"
		controllerName = DefaultNetworkControllerName
	)

	getFakeController := func(nbClient libovsdbclient.Client) *DefaultNetworkController {
		controller := getFakeController(DefaultNetworkControllerName)
		controller.nbClient = nbClient
		return controller
	}

	ginkgo.BeforeEach(func() {
		nbCleanup = nil
		logicalSwitch = &nbdb.LogicalSwitch{
			Name: nodeName,
			UUID: nodeName + "_UUID",
		}
	})

	ginkgo.AfterEach(func() {
		if nbCleanup != nil {
			nbCleanup.Cleanup()
		}
	})

	for _, ipMode := range []string{"ipv4", "ipv6"} {
		var mgmtIP string
		if ipMode == "ipv4" {
			mgmtIP = ipv4MgmtIP
		} else {
			mgmtIP = ipv6MgmtIP
		}
		ginkgo.It(fmt.Sprintf("sync existing ACLs on startup, %s mode", ipMode), func() {
			// mock existing management port
			mgmtPortMAC := "0a:58:0a:01:01:02"
			mgmtPort := &nbdb.LogicalSwitchPort{
				Name:      types.K8sPrefix + nodeName,
				UUID:      types.K8sPrefix + nodeName + "-UUID",
				Type:      "",
				Options:   nil,
				Addresses: []string{mgmtPortMAC + " " + mgmtIP},
			}
			logicalSwitch.Ports = []string{mgmtPort.UUID}
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					getAllowFromNodeStaleACL(nodeName, mgmtIP, logicalSwitch, controllerName),
					logicalSwitch,
					mgmtPort,
				},
			}
			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeController := getFakeController(nbClient)
			err = fakeController.addAllowACLFromNode(nodeName, net.ParseIP(mgmtIP))

			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedData := []libovsdb.TestData{
				logicalSwitch,
				mgmtPort,
				getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName),
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})
		ginkgo.It(fmt.Sprintf("adding an existing ACL to the node switch, %s mode", ipMode), func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
					getAllowFromNodeExpectedACL(nodeName, mgmtIP, nil, controllerName),
				},
			}
			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeController := getFakeController(nbClient)
			err = fakeController.addAllowACLFromNode(nodeName, ovntest.MustParseIP(mgmtIP))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedData := []libovsdb.TestData{
				logicalSwitch,
				getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName),
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})

		ginkgo.It(fmt.Sprintf("creating new ACL and adding it to node switch, %s mode", ipMode), func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeController := getFakeController(nbClient)
			err = fakeController.addAllowACLFromNode(nodeName, ovntest.MustParseIP(mgmtIP))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedData := []libovsdb.TestData{
				logicalSwitch,
				getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName),
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})
	}
})

var _ = ginkgo.Describe("OVN NetworkPolicy Low-Level Operations", func() {
	var asFactory *addressset.FakeAddressSetFactory

	buildExpectedIngressPeerNSv4ACL := func(gp *gressPolicy, pgName string, asDbIDses []*libovsdbops.DbObjectIDs,
		aclLogging *ACLLoggingLevels) *nbdb.ACL {
		aclIDs := gp.getNetpolACLDbIDs(emptyIdx, emptyIdx)
		hashedASNames := []string{}

		for _, dbIdx := range asDbIDses {
			hashedASName, _ := addressset.GetHashNamesForAS(dbIdx)
			hashedASNames = append(hashedASNames, hashedASName)
		}
		asMatch := asMatch(hashedASNames)
		match := fmt.Sprintf("ip4.src == {%s} && outport == @%s", asMatch, pgName)
		acl := libovsdbops.BuildACL(getACLName(aclIDs), nbdb.ACLDirectionToLport, types.DefaultAllowPriority, match,
			nbdb.ACLActionAllowRelated, types.OvnACLLoggingMeter, aclLogging.Allow, true, aclIDs.GetExternalIDs(), nil)
		return acl
	}

	ginkgo.It("computes match strings from address sets correctly", func() {
		const (
			pgName         string = "pg-name"
			controllerName        = DefaultNetworkControllerName
		)
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		asFactory = addressset.NewFakeAddressSetFactory(controllerName)
		config.IPv4Mode = true
		config.IPv6Mode = false
		asIDs := getPodSelectorAddrSetDbIDs("test_name", DefaultNetworkControllerName)
		gp := newGressPolicy(knet.PolicyTypeIngress, 0, "testing", "policy", controllerName,
			false, &util.DefaultNetConfInfo{})
		gp.hasPeerSelector = true
		gp.addPeerAddressSets(addressset.GetHashNamesForAS(asIDs))

		one := getNamespaceAddrSetDbIDs("ns1", controllerName)
		two := getNamespaceAddrSetDbIDs("ns2", controllerName)
		three := getNamespaceAddrSetDbIDs("ns3", controllerName)
		four := getNamespaceAddrSetDbIDs("ns4", controllerName)
		five := getNamespaceAddrSetDbIDs("ns5", controllerName)
		six := getNamespaceAddrSetDbIDs("ns6", controllerName)
		for _, addrSetID := range []*libovsdbops.DbObjectIDs{one, two, three, four, five, six} {
			_, err := asFactory.EnsureAddressSet(addrSetID)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		defaultAclLogging := ACLLoggingLevels{
			nbdb.ACLSeverityInfo,
			nbdb.ACLSeverityInfo,
		}

		gomega.Expect(gp.addNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected := buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one}, &defaultAclLogging)
		actual, _ := gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.addNamespaceAddressSet(two.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// address sets should be alphabetized
		gomega.Expect(gp.addNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// re-adding an existing set is a no-op
		gomega.Expect(gp.addNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// now delete a set
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, three, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is a no-op
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())

		// add and delete some more...
		gomega.Expect(gp.addNamespaceAddressSet(five.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, three, four, five}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, four, five}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(six.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, four, five, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(two.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four, five, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(five.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(six.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())
	})
})
