// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package local_test

import (
	"fmt"
	"testing"

	"github.com/openconfig/lemming/bgp"
	"github.com/openconfig/lemming/gnmi/oc"
	"github.com/openconfig/lemming/gnmi/oc/ocpath"
	"github.com/openconfig/lemming/policytest"
)

func TestPrefixSetMode(t *testing.T) {
	dut1, stop1 := newLemming(t, 1, 64500, []*AddIntfAction{{
		name:    "eth0",
		ifindex: 0,
		enabled: true,
		prefix:  "192.0.2.1/31",
		niName:  "DEFAULT",
	}})
	defer stop1()

	prefix1 := "10.33.0.0/16"
	prefix2 := "10.34.0.0/16"
	prefix2v6 := "10::34/16"

	// Create prefix set
	prefixSetName := "reject-" + prefix1
	prefixSetPath := ocpath.Root().RoutingPolicy().DefinedSets().PrefixSet(prefixSetName)
	Replace(t, dut1, prefixSetPath.Mode().Config(), oc.PrefixSet_Mode_IPV4)
	Replace(t, dut1, prefixSetPath.Prefix(prefix1, "exact").IpPrefix().Config(), prefix1)
	ReplaceExpectFail(t, dut1, prefixSetPath.Prefix(prefix2v6, "exact").IpPrefix().Config(), prefix2v6)
	Replace(t, dut1, prefixSetPath.Prefix(prefix2, "exact").IpPrefix().Config(), prefix2)
}

func TestPrefixSet(t *testing.T) {
	installPolicies := func(t *testing.T, dut1, dut2, _, _, _ *Device, invert bool) {
		if debug {
			fmt.Println("Installing test policies")
		}
		prefix1 := "10.33.0.0/16"
		prefix2 := "10.34.0.0/16"
		prefix3 := "10.0.6.0/24"

		// Policy to reject routes with the given prefix set
		policyName := "def1"

		// Create prefix set
		prefixSetName := "reject-" + prefix1
		prefixSetPath := ocpath.Root().RoutingPolicy().DefinedSets().PrefixSet(prefixSetName)
		Replace(t, dut2, prefixSetPath.Mode().Config(), oc.PrefixSet_Mode_IPV4)
		prefix1Path := prefixSetPath.Prefix(prefix1, "exact").IpPrefix()
		Replace(t, dut2, prefix1Path.Config(), prefix1)
		prefix2Path := prefixSetPath.Prefix(prefix2, "16..23").IpPrefix()
		Replace(t, dut2, prefix2Path.Config(), prefix2)
		prefix3Path := prefixSetPath.Prefix(prefix3, "28..28").IpPrefix()
		Replace(t, dut2, prefix3Path.Config(), prefix3)

		policy := &oc.RoutingPolicy_PolicyDefinition_Statement_OrderedMap{}
		stmt, err := policy.AppendNew("stmt1")
		if err != nil {
			t.Fatalf("Cannot append new BGP policy statement: %v", err)
		}
		// Match on prefix set & reject route
		stmt.GetOrCreateConditions().GetOrCreateMatchPrefixSet().SetPrefixSet(prefixSetName)
		if invert {
			stmt.GetOrCreateConditions().GetOrCreateMatchPrefixSet().SetMatchSetOptions(oc.PolicyTypes_MatchSetOptionsRestrictedType_INVERT)
		} else {
			stmt.GetOrCreateConditions().GetOrCreateMatchPrefixSet().SetMatchSetOptions(oc.PolicyTypes_MatchSetOptionsRestrictedType_ANY)
		}
		stmt.GetOrCreateActions().SetPolicyResult(oc.RoutingPolicy_PolicyResultType_REJECT_ROUTE)
		// Install policy
		Replace(t, dut2, ocpath.Root().RoutingPolicy().PolicyDefinition(policyName).Config(), &oc.RoutingPolicy_PolicyDefinition{Statement: policy})
		Replace(t, dut2, bgp.BGPPath.Neighbor(dut1.RouterID).ApplyPolicy().ImportPolicy().Config(), []string{policyName})
		Await(t, dut2, bgp.BGPPath.Neighbor(dut1.RouterID).ApplyPolicy().ImportPolicy().State(), []string{policyName})
	}

	invertResult := func(result policytest.RouteTestResult, invert bool) policytest.RouteTestResult {
		if invert {
			switch result {
			case policytest.RouteAccepted:
				return policytest.RouteDiscarded
			case policytest.RouteDiscarded:
				return policytest.RouteAccepted
			default:
			}
		}
		return result
	}

	getspec := func(invert bool) *PolicyTestCase {
		return &PolicyTestCase{
			description:         "Test that one prefix gets accepted and the other rejected via an ANY prefix-set.",
			skipValidateAttrSet: true,
			routeTests: []*policytest.RouteTestCase{{
				Description: "Exact match",
				Input: policytest.TestRoute{
					ReachPrefix: "10.33.0.0/16",
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "Not exact match",
				Input: policytest.TestRoute{
					ReachPrefix: "10.33.0.0/17",
				},
				ExpectedResult: invertResult(policytest.RouteAccepted, invert),
			}, {
				Description: "No match with any prefix",
				Input: policytest.TestRoute{
					ReachPrefix: "10.3.0.0/16",
				},
				ExpectedResult: invertResult(policytest.RouteAccepted, invert),
			}, {
				Description: "mask length too short",
				Input: policytest.TestRoute{
					ReachPrefix: "10.34.0.0/15",
				},
				ExpectedResult: invertResult(policytest.RouteAccepted, invert),
			}, {
				Description: "Lower end of mask length",
				Input: policytest.TestRoute{
					ReachPrefix: "10.34.0.0/16",
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "Middle of mask length",
				Input: policytest.TestRoute{
					ReachPrefix: "10.34.0.0/20",
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "Middle of mask length -- different prefix",
				Input: policytest.TestRoute{
					ReachPrefix: "10.34.240.0/20",
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "Upper end of mask length",
				Input: policytest.TestRoute{
					ReachPrefix: "10.34.0.0/23",
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "Upper end of mask length -- different prefix",
				Input: policytest.TestRoute{
					ReachPrefix: "10.34.254.0/23",
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "mask length too long",
				Input: policytest.TestRoute{
					ReachPrefix: "10.34.0.0/24",
				},
				ExpectedResult: invertResult(policytest.RouteAccepted, invert),
			}, {
				Description: "eq-prefix-lowest",
				Input: policytest.TestRoute{
					ReachPrefix: "10.0.6.0/28",
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "eq-prefix-middle",
				Input: policytest.TestRoute{
					ReachPrefix: "10.0.6.192/28", // 192 = 0xc0
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}, {
				Description: "eq-prefix-no-match",
				Input: policytest.TestRoute{
					ReachPrefix: "10.0.7.192/28", // 192 = 0xc0
				},
				ExpectedResult: invertResult(policytest.RouteAccepted, invert),
			}, {
				Description: "eq-prefix-highest",
				Input: policytest.TestRoute{
					ReachPrefix: "10.0.6.240/28", // 240 = 0xf0
				},
				ExpectedResult: invertResult(policytest.RouteDiscarded, invert),
			}},
			installPolicies: func(t *testing.T, dut1, dut2, dut3, dut4, dut5 *Device) {
				installPolicies(t, dut1, dut2, dut3, dut4, dut5, invert)
			},
		}
	}

	t.Run("ANY", func(t *testing.T) {
		testPolicy(t, getspec(false))
	})
	t.Run("INVERT", func(t *testing.T) {
		testPolicy(t, getspec(true))
	})
}
