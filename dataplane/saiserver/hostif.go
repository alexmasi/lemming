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

package saiserver

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/openconfig/lemming/dataplane/dplaneopts"
	"github.com/openconfig/lemming/dataplane/forwarding/attributes"
	"github.com/openconfig/lemming/dataplane/forwarding/fwdconfig"
	"github.com/openconfig/lemming/dataplane/forwarding/fwdport/ports"
	"github.com/openconfig/lemming/dataplane/saiserver/attrmgr"

	log "github.com/golang/glog"

	pktiopb "github.com/openconfig/lemming/dataplane/proto/packetio"
	saipb "github.com/openconfig/lemming/dataplane/proto/sai"
	fwdpb "github.com/openconfig/lemming/proto/forwarding"
)

func newHostif(mgr *attrmgr.AttrMgr, dataplane switchDataplaneAPI, s *grpc.Server, opts *dplaneopts.Options) *hostif {
	hostif := &hostif{
		mgr:              mgr,
		dataplane:        dataplane,
		trapIDToHostifID: map[uint64]uint64{},
		groupIDToQueue:   map[uint64]uint32{},
		remoteHostifs:    map[uint64]*pktiopb.HostPortControlMessage{},
		opts:             opts,
	}

	saipb.RegisterHostifServer(s, hostif)
	pktiopb.RegisterPacketIOServer(s, hostif)
	return hostif
}

type hostif struct {
	saipb.UnimplementedHostifServer
	mgr              *attrmgr.AttrMgr
	dataplane        switchDataplaneAPI
	trapIDToHostifID map[uint64]uint64
	groupIDToQueue   map[uint64]uint32
	opts             *dplaneopts.Options
	remoteMu         sync.Mutex
	remoteHostifs    map[uint64]*pktiopb.HostPortControlMessage
	remoteClosers    []func()
	remotePortReq    func(msg *pktiopb.HostPortControlMessage) error
}

func (hostif *hostif) Reset() {
	log.Info("resetting hostif")
	for _, closeFn := range hostif.remoteClosers {
		closeFn()
	}
	hostif.remoteClosers = nil
	hostif.trapIDToHostifID = map[uint64]uint64{}
	hostif.groupIDToQueue = map[uint64]uint32{}
	hostif.remoteHostifs = map[uint64]*pktiopb.HostPortControlMessage{}
	hostif.remotePortReq = nil
}

const switchID = 1

// CreateHostif creates a hostif interface (usually a tap interface).
func (hostif *hostif) CreateHostif(ctx context.Context, req *saipb.CreateHostifRequest) (*saipb.CreateHostifResponse, error) {
	if hostif.opts.RemoteCPUPort {
		return hostif.createRemoteHostif(ctx, req)
	}
	id := hostif.mgr.NextID()

	switch req.GetType() {
	case saipb.HostifType_HOSTIF_TYPE_GENETLINK: // For genetlink device, pass the port description to the cpu sink.
		// First, create the port in the dataplane: this port is "virtual", it doesn't do any packet io.
		// It ensures that the packet metadata is correct.
		portReq := &fwdpb.PortCreateRequest{
			ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
			Port: &fwdpb.PortDesc{
				PortId:   &fwdpb.PortId{ObjectId: &fwdpb.ObjectId{Id: fmt.Sprint(id)}},
				PortType: fwdpb.PortType_PORT_TYPE_GENETLINK,
				Port: &fwdpb.PortDesc_Genetlink{
					Genetlink: &fwdpb.GenetlinkPortDesc{
						FamilyName: string(req.GetName()),
						GroupName:  string(req.GetGenetlinkMcgrpName()),
					},
				},
			},
		}
		if _, err := hostif.dataplane.PortCreate(ctx, portReq); err != nil {
			return nil, err
		}
		// Notify the cpu sink about these port types.
		fwdCtx, err := hostif.dataplane.FindContext(&fwdpb.ContextId{Id: hostif.dataplane.ID()})
		if err != nil {
			return nil, err
		}
		fwdCtx.RLock()
		ps := fwdCtx.PacketSink()
		fwdCtx.RUnlock()
		ps(&fwdpb.PacketSinkResponse{
			Resp: &fwdpb.PacketSinkResponse_Port{
				Port: &fwdpb.PacketSinkPortInfo{
					Port: portReq.Port,
				},
			},
		})

		return &saipb.CreateHostifResponse{Oid: id}, nil
	case saipb.HostifType_HOSTIF_TYPE_NETDEV:
		portType := hostif.opts.HostifNetDevType
		port := &fwdpb.PortCreateRequest{
			ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
			Port: &fwdpb.PortDesc{
				PortType: portType,
				PortId: &fwdpb.PortId{
					ObjectId: &fwdpb.ObjectId{Id: fmt.Sprint(id)},
				},
			},
		}
		switch portType {
		case fwdpb.PortType_PORT_TYPE_KERNEL:
			port.Port.Port = &fwdpb.PortDesc_Kernel{
				Kernel: &fwdpb.KernelPortDesc{
					DeviceName: string(req.GetName()),
				},
			}
		case fwdpb.PortType_PORT_TYPE_TAP:
			port.Port.Port = &fwdpb.PortDesc_Tap{
				Tap: &fwdpb.TAPPortDesc{
					DeviceName: string(req.GetName()),
				},
			}
		default:
			return nil, fmt.Errorf("unkown port type: %v", portType)
		}

		if _, err := hostif.dataplane.PortCreate(ctx, port); err != nil {
			if err != nil {
				return nil, err
			}
		}

		attrReq := &saipb.GetPortAttributeRequest{Oid: req.GetObjId(), AttrType: []saipb.PortAttr{saipb.PortAttr_PORT_ATTR_OPER_STATUS}}
		p := &saipb.GetPortAttributeResponse{}
		if err := hostif.mgr.PopulateAttributes(attrReq, p); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get cpu port: %v", err)
		}
		// If there is a corresponding port for the hostif, update the attributes
		if p.GetAttr().GetOperStatus() != saipb.PortOperStatus_PORT_OPER_STATUS_NOT_PRESENT {
			_, err := hostif.dataplane.AttributeUpdate(ctx, &fwdpb.AttributeUpdateRequest{
				ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
				ObjectId:  &fwdpb.ObjectId{Id: fmt.Sprint(id)},
				AttrId:    attributes.SwapActionRelatedPort,
				AttrValue: fmt.Sprint(req.GetObjId()),
			})
			if err != nil {
				return nil, err
			}
			_, err = hostif.dataplane.AttributeUpdate(ctx, &fwdpb.AttributeUpdateRequest{
				ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
				ObjectId:  &fwdpb.ObjectId{Id: fmt.Sprint(req.GetObjId())},
				AttrId:    attributes.SwapActionRelatedPort,
				AttrValue: fmt.Sprint(id),
			})
			if err != nil {
				return nil, err
			}
		}

		cpuPortReq := &saipb.GetSwitchAttributeRequest{Oid: switchID, AttrType: []saipb.SwitchAttr{saipb.SwitchAttr_SWITCH_ATTR_CPU_PORT}}
		resp := &saipb.GetSwitchAttributeResponse{}
		if err := hostif.mgr.PopulateAttributes(cpuPortReq, resp); err != nil {
			return nil, err
		}

		// Packets received from hostif are sent to their corresponding port.
		update := &fwdpb.PortUpdateRequest{
			ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
			PortId:    &fwdpb.PortId{ObjectId: &fwdpb.ObjectId{Id: fmt.Sprint(id)}},
			Update: &fwdpb.PortUpdateDesc{
				Port: &fwdpb.PortUpdateDesc_Kernel{
					Kernel: &fwdpb.KernelPortUpdateDesc{
						Inputs: []*fwdpb.ActionDesc{{
							ActionType: fwdpb.ActionType_ACTION_TYPE_SWAP_OUTPUT_INTERNAL_EXTERNAL,
						}, {
							ActionType: fwdpb.ActionType_ACTION_TYPE_OUTPUT,
						}},
					},
				},
			},
		}

		// Unless, the corresponding port for this hostif is the CPU port, then run the normal forwarding pipeline.
		if resp.GetAttr().GetCpuPort() == req.GetObjId() {
			update.Update.GetKernel().Inputs = getForwardingPipeline()
		}

		if _, err := hostif.dataplane.PortUpdate(ctx, update); err != nil {
			return nil, err
		}

		attr := &saipb.HostifAttribute{
			OperStatus: proto.Bool(true),
		}
		hostif.mgr.StoreAttributes(id, attr)

		// Notify the cpu sink about these port types, if there is one configured.
		fwdCtx, err := hostif.dataplane.FindContext(&fwdpb.ContextId{Id: hostif.dataplane.ID()})
		if err != nil {
			return nil, err
		}
		fwdCtx.RLock()
		ps := fwdCtx.PacketSink()
		fwdCtx.RUnlock()
		if ps != nil {
			ps(&fwdpb.PacketSinkResponse{
				Resp: &fwdpb.PacketSinkResponse_Port{
					Port: &fwdpb.PacketSinkPortInfo{
						Port: &fwdpb.PortDesc{
							PortType: fwdpb.PortType_PORT_TYPE_KERNEL,
							PortId:   &fwdpb.PortId{ObjectId: &fwdpb.ObjectId{Id: fmt.Sprint(id)}},
							Port: &fwdpb.PortDesc_Kernel{
								Kernel: &fwdpb.KernelPortDesc{DeviceName: string(req.GetName())},
							},
						},
					},
				},
			})
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown type %v", req.GetType())
	}
	return &saipb.CreateHostifResponse{Oid: id}, nil
}

func (hostif *hostif) createRemoteHostif(ctx context.Context, req *saipb.CreateHostifRequest) (*saipb.CreateHostifResponse, error) {
	id := hostif.mgr.NextID()

	ctlReq := &pktiopb.HostPortControlMessage{
		Create:        true,
		DataplanePort: req.GetObjId(),
		PortId:        id,
	}

	switch req.GetType() {
	case saipb.HostifType_HOSTIF_TYPE_GENETLINK:
		ctlReq.Port = &pktiopb.HostPortControlMessage_Genetlink{
			Genetlink: &pktiopb.GenetlinkPort{
				Family: string(req.GetName()),
				Group:  string(req.GetGenetlinkMcgrpName()),
			},
		}
	case saipb.HostifType_HOSTIF_TYPE_NETDEV:
		ctlReq.Port = &pktiopb.HostPortControlMessage_Netdev{
			Netdev: &pktiopb.NetdevPort{
				Name: string(req.GetName()),
			},
		}

		// For packets coming from a netdev hostif, send them out its corresponding port.
		cpuPortReq := &saipb.GetSwitchAttributeRequest{Oid: switchID, AttrType: []saipb.SwitchAttr{saipb.SwitchAttr_SWITCH_ATTR_CPU_PORT}}
		resp := &saipb.GetSwitchAttributeResponse{}
		if err := hostif.mgr.PopulateAttributes(cpuPortReq, resp); err != nil {
			return nil, err
		}
		entry := fwdconfig.TableEntryAddRequest(hostif.dataplane.ID(), hostifToPortTable).
			AppendEntry(fwdconfig.EntryDesc(fwdconfig.ExactEntry(fwdconfig.PacketFieldBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_HOST_PORT_ID).WithUint64(id))),
				fwdconfig.Action(fwdconfig.TransmitAction(fmt.Sprint(req.GetObjId())))).Build()

		if req.GetObjId() == resp.GetAttr().GetCpuPort() {
			entry.Entries[0].Actions = getForwardingPipeline()
		}

		if _, err := hostif.dataplane.TableEntryAdd(ctx, entry); err != nil {
			return nil, err
		}

		nid, err := hostif.dataplane.ObjectNID(ctx, &fwdpb.ObjectNIDRequest{
			ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
			ObjectId:  &fwdpb.ObjectId{Id: fmt.Sprint(req.GetObjId())},
		})
		if err != nil {
			return nil, err
		}

		entry = fwdconfig.TableEntryAddRequest(hostif.dataplane.ID(), portToHostifTable).
			AppendEntry(fwdconfig.EntryDesc(fwdconfig.ExactEntry(fwdconfig.PacketFieldBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_PACKET_PORT_INPUT).WithUint64(nid.GetNid()))),
				fwdconfig.Action(fwdconfig.UpdateAction(fwdpb.UpdateType_UPDATE_TYPE_SET, fwdpb.PacketFieldNum_PACKET_FIELD_NUM_HOST_PORT_ID).WithUint64Value(id))).Build()

		if _, err := hostif.dataplane.TableEntryAdd(ctx, entry); err != nil {
			return nil, err
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown type %v", req.GetType())
	}

	hostif.remoteMu.Lock()
	defer hostif.remoteMu.Unlock()

	if hostif.remotePortReq == nil {
		return nil, status.Error(codes.FailedPrecondition, "remote port control not configured")
	}
	if err := hostif.remotePortReq(ctlReq); err != nil {
		return nil, err
	}

	attr := &saipb.HostifAttribute{
		OperStatus: proto.Bool(true),
	}
	hostif.mgr.StoreAttributes(id, attr)
	hostif.remoteHostifs[id] = ctlReq

	return &saipb.CreateHostifResponse{Oid: id}, nil
}

func (hostif *hostif) RemoveHostif(ctx context.Context, req *saipb.RemoveHostifRequest) (*saipb.RemoveHostifResponse, error) {
	if !hostif.opts.RemoteCPUPort {
		return nil, status.Error(codes.FailedPrecondition, "only remote cpu port is supported")
	}
	hostif.remoteMu.Lock()
	defer hostif.remoteMu.Unlock()

	nid, err := hostif.dataplane.ObjectNID(ctx, &fwdpb.ObjectNIDRequest{
		ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
		ObjectId:  &fwdpb.ObjectId{Id: fmt.Sprint(hostif.remoteHostifs[req.GetOid()].GetDataplanePort())},
	})
	if err != nil {
		return nil, err
	}

	delReq := fwdconfig.TableEntryRemoveRequest(hostif.dataplane.ID(), hostifToPortTable).AppendEntry(
		fwdconfig.EntryDesc(fwdconfig.ExactEntry(fwdconfig.PacketFieldBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_HOST_PORT_ID).WithUint64(req.GetOid()))),
	).Build()
	if _, err := hostif.dataplane.TableEntryRemove(ctx, delReq); err != nil {
		return nil, err
	}

	delReq = fwdconfig.TableEntryRemoveRequest(hostif.dataplane.ID(), portToHostifTable).AppendEntry(
		fwdconfig.EntryDesc(fwdconfig.ExactEntry(fwdconfig.PacketFieldBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_PACKET_PORT_INPUT).WithUint64(nid.GetNid()))),
	).Build()
	if _, err := hostif.dataplane.TableEntryRemove(ctx, delReq); err != nil {
		return nil, err
	}

	ctlReq := &pktiopb.HostPortControlMessage{
		Create: false,
		PortId: req.Oid,
	}

	if hostif.remotePortReq == nil {
		return nil, status.Error(codes.FailedPrecondition, "remote port control not configured")
	}
	if err := hostif.remotePortReq(ctlReq); err != nil {
		return nil, err
	}
	delete(hostif.remoteHostifs, req.Oid)

	return &saipb.RemoveHostifResponse{}, nil
}

// SetHostifAttribute sets the attributes in the request.
func (hostif *hostif) SetHostifAttribute(ctx context.Context, req *saipb.SetHostifAttributeRequest) (*saipb.SetHostifAttributeResponse, error) {
	if req.OperStatus != nil {
		if hostif.opts.RemoteCPUPort {
			return nil, nil
		}
		stateReq := &fwdpb.PortStateRequest{
			ContextId: &fwdpb.ContextId{Id: hostif.dataplane.ID()},
			PortId:    &fwdpb.PortId{ObjectId: &fwdpb.ObjectId{Id: fmt.Sprint(req.GetOid())}},
		}
		stateReq.Operation = &fwdpb.PortInfo{
			AdminStatus: fwdpb.PortState_PORT_STATE_DISABLED_DOWN,
		}
		if req.GetOperStatus() {
			stateReq.Operation.AdminStatus = fwdpb.PortState_PORT_STATE_ENABLED_UP
		}
		_, err := hostif.dataplane.PortState(ctx, stateReq)
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// SetHostifTrapGroupAttribute sets the trap group attribute.
func (*hostif) SetHostifTrapGroupAttribute(context.Context, *saipb.SetHostifTrapGroupAttributeRequest) (*saipb.SetHostifTrapGroupAttributeResponse, error) {
	// TODO: provide implementation.
	return &saipb.SetHostifTrapGroupAttributeResponse{}, nil
}

var (
	etherTypeARP  = []byte{0x08, 0x06}
	udldDstMAC    = []byte{0x01, 0x00, 0x0C, 0xCC, 0xCC, 0xCC}
	etherTypeLLDP = []byte{0x88, 0xcc}
	ndDstMAC      = []byte{0x33, 0x33, 0x00, 0x00, 0x00, 0x00} // ND is generic IPv6 multicast MAC.
	ndDstMACMask  = []byte{0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00}
	lacpDstMAC    = []byte{0x01, 0x80, 0xC2, 0x00, 0x00, 0x02}
)

const (
	bgpPort        = 179
	trapTableID    = "trap-table"
	wildcardPortID = 0
)

func (hostif *hostif) CreateHostifTrap(ctx context.Context, req *saipb.CreateHostifTrapRequest) (*saipb.CreateHostifTrapResponse, error) {
	id := hostif.mgr.NextID()
	fwdReq := fwdconfig.TableEntryAddRequest(hostif.dataplane.ID(), trapTableID)

	swReq := &saipb.GetSwitchAttributeRequest{
		Oid:      req.GetSwitch(),
		AttrType: []saipb.SwitchAttr{saipb.SwitchAttr_SWITCH_ATTR_CPU_PORT},
	}
	swAttr := &saipb.GetSwitchAttributeResponse{}
	if err := hostif.mgr.PopulateAttributes(swReq, swAttr); err != nil {
		return nil, err
	}
	entriesAdded := 1
	switch tType := req.GetTrapType(); tType {
	case saipb.HostifTrapType_HOSTIF_TRAP_TYPE_ARP_REQUEST, saipb.HostifTrapType_HOSTIF_TRAP_TYPE_ARP_RESPONSE:
		fwdReq.AppendEntry(fwdconfig.EntryDesc(fwdconfig.FlowEntry(
			fwdconfig.PacketFieldMaskedBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_ETHER_TYPE).
				WithBytes(etherTypeARP, []byte{0xFF, 0xFF}))))
	case saipb.HostifTrapType_HOSTIF_TRAP_TYPE_UDLD:
		fwdReq.AppendEntry(fwdconfig.EntryDesc(fwdconfig.FlowEntry(
			fwdconfig.PacketFieldMaskedBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_ETHER_MAC_DST).
				WithBytes(udldDstMAC, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}))))
	case saipb.HostifTrapType_HOSTIF_TRAP_TYPE_LLDP:
		fwdReq.AppendEntry(fwdconfig.EntryDesc(fwdconfig.FlowEntry(
			fwdconfig.PacketFieldMaskedBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_ETHER_TYPE).
				WithBytes(etherTypeLLDP, []byte{0xFF, 0xFF}))))
	case saipb.HostifTrapType_HOSTIF_TRAP_TYPE_IPV6_NEIGHBOR_DISCOVERY:
		fwdReq.AppendEntry(fwdconfig.EntryDesc(fwdconfig.FlowEntry(
			fwdconfig.PacketFieldMaskedBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_ETHER_MAC_DST).
				WithBytes(ndDstMAC, ndDstMACMask))))
	case saipb.HostifTrapType_HOSTIF_TRAP_TYPE_LACP:
		fwdReq.AppendEntry(fwdconfig.EntryDesc(fwdconfig.FlowEntry(
			fwdconfig.PacketFieldMaskedBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_ETHER_MAC_DST).
				WithBytes(lacpDstMAC, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}))))
	case saipb.HostifTrapType_HOSTIF_TRAP_TYPE_IP2ME:
		// IP2ME routes are added to the FIB, do nothing here.
		return &saipb.CreateHostifTrapResponse{
			Oid: id,
		}, nil
	case saipb.HostifTrapType_HOSTIF_TRAP_TYPE_BGP, saipb.HostifTrapType_HOSTIF_TRAP_TYPE_BGPV6:
		// TODO: This should only match for packets destined to the management IP.
		fwdReq.AppendEntry(fwdconfig.EntryDesc(fwdconfig.FlowEntry(
			fwdconfig.PacketFieldMaskedBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_L4_PORT_SRC).
				WithUint16(bgpPort))),
		)
		fwdReq.AppendEntry(fwdconfig.EntryDesc(fwdconfig.FlowEntry(
			fwdconfig.PacketFieldMaskedBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_L4_PORT_DST).
				WithUint16(bgpPort))),
		)
		entriesAdded = 2
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown trap type: %v", tType)
	}

	switch act := req.GetPacketAction(); act { // TODO: Support copy
	case saipb.PacketAction_PACKET_ACTION_TRAP, saipb.PacketAction_PACKET_ACTION_COPY: // TRAP means COPY to CPU and DROP, just transmit immediately, which interrupts any pending actions.
		for i := 0; i < entriesAdded; i++ {
			fwdReq.AppendActions(fwdconfig.Action(fwdconfig.TransmitAction(fmt.Sprint(swAttr.GetAttr().GetCpuPort())).WithImmediate(true)))
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown action type: %v", act)
	}
	if _, err := hostif.dataplane.TableEntryAdd(ctx, fwdReq.Build()); err != nil {
		return nil, err
	}
	// TODO: Support multiple queues, by using the group ID.
	return &saipb.CreateHostifTrapResponse{
		Oid: id,
	}, nil
}

func (hostif *hostif) CreateHostifTrapGroup(_ context.Context, req *saipb.CreateHostifTrapGroupRequest) (*saipb.CreateHostifTrapGroupResponse, error) {
	id := hostif.mgr.NextID()
	hostif.groupIDToQueue[id] = req.GetQueue()
	return &saipb.CreateHostifTrapGroupResponse{Oid: id}, nil
}

func (hostif *hostif) CreateHostifUserDefinedTrap(_ context.Context, req *saipb.CreateHostifUserDefinedTrapRequest) (*saipb.CreateHostifUserDefinedTrapResponse, error) {
	if req.GetType() != saipb.HostifUserDefinedTrapType_HOSTIF_USER_DEFINED_TRAP_TYPE_ACL {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported trap type: %v", req.GetType())
	}
	return &saipb.CreateHostifUserDefinedTrapResponse{Oid: hostif.mgr.NextID()}, nil
}

const (
	trapIDToHostifTable = "hostiftable"
)

func (hostif *hostif) CreateHostifTableEntry(ctx context.Context, req *saipb.CreateHostifTableEntryRequest) (*saipb.CreateHostifTableEntryResponse, error) {
	switch entryType := req.GetType(); entryType {
	case saipb.HostifTableEntryType_HOSTIF_TABLE_ENTRY_TYPE_TRAP_ID:
		hostif.trapIDToHostifID[req.GetTrapId()] = req.GetHostIf()
		_, err := hostif.dataplane.TableEntryAdd(ctx, fwdconfig.TableEntryAddRequest(hostif.dataplane.ID(), trapIDToHostifTable).
			AppendEntry(
				fwdconfig.EntryDesc(fwdconfig.ExactEntry(
					fwdconfig.PacketFieldBytes(fwdpb.PacketFieldNum_PACKET_FIELD_NUM_TRAP_ID).WithUint64(req.GetTrapId()))),
				fwdconfig.Action(fwdconfig.UpdateAction(fwdpb.UpdateType_UPDATE_TYPE_SET, fwdpb.PacketFieldNum_PACKET_FIELD_NUM_HOST_PORT_ID).WithUint64Value(req.GetHostIf()))).
			Build())
		if err != nil {
			return nil, err
		}
	case saipb.HostifTableEntryType_HOSTIF_TABLE_ENTRY_TYPE_WILDCARD:
		hostif.trapIDToHostifID[req.GetTrapId()] = wildcardPortID
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported entry type: %v", entryType)
	}

	return nil, nil
}

func (hostif *hostif) CPUPacketStream(srv pktiopb.PacketIO_CPUPacketStreamServer) error {
	_, err := srv.Recv()
	if err != nil {
		return err
	}
	fwdCtx, err := hostif.dataplane.FindContext(&fwdpb.ContextId{Id: hostif.dataplane.ID()})
	if err != nil {
		return err
	}

	fwdCtx.RLock()
	cpuPortID := ""
	for _, id := range fwdCtx.Objects.IDs() {
		obj, err := fwdCtx.Objects.FindID(&fwdpb.ObjectId{Id: string(id)})
		if err != nil {
			fwdCtx.RUnlock()
			return err
		}
		if _, ok := obj.(*ports.CPUPort); ok {
			cpuPortID = string(obj.ID())
		}
	}
	fwdCtx.RUnlock()
	if cpuPortID == "" {
		return fmt.Errorf("couldn't find cpu port")
	}

	packetCh := make(chan *pktiopb.PacketIn)
	ctx, cancel := context.WithCancel(srv.Context())

	// Since Recv() is blocking and we want this func to return immediately on cancel.
	// Run the Recv in a seperate goroutine.
	go func() {
		for {
			pkt, err := srv.Recv()
			if err != nil {
				continue
			}
			packetCh <- pkt
		}
	}()

	fn := func(po *pktiopb.PacketOut) error {
		return srv.Send(po)
	}

	fwdCtx.Lock()
	fwdCtx.SetCPUPortSink(fn, cancel)
	fwdCtx.Unlock()

	for {
		select {
		case <-ctx.Done():
			return nil
		case pkt := <-packetCh:
			acts := []*fwdpb.ActionDesc{fwdconfig.Action(fwdconfig.UpdateAction(fwdpb.UpdateType_UPDATE_TYPE_SET, fwdpb.PacketFieldNum_PACKET_FIELD_NUM_HOST_PORT_ID).
				WithUint64Value(pkt.GetPacket().GetHostPort())).Build()}
			err = hostif.dataplane.InjectPacket(&fwdpb.ContextId{Id: hostif.dataplane.ID()}, &fwdpb.PortId{ObjectId: &fwdpb.ObjectId{Id: cpuPortID}}, fwdpb.PacketHeaderId_PACKET_HEADER_ID_ETHERNET,
				pkt.GetPacket().GetFrame(), acts, true, fwdpb.PortAction_PORT_ACTION_INPUT)
			if err != nil {
				log.Warningf("inject err: %v", err)
				continue
			}
		}
	}
}

func (hostif *hostif) HostPortControl(srv pktiopb.PacketIO_HostPortControlServer) error {
	log.Info("started host port control channel")
	_, err := srv.Recv()
	if err != nil {
		return err
	}
	log.Info("received init port control channel")

	hostif.remoteMu.Lock()
	ctx, cancelFn := context.WithCancel(srv.Context())
	hostif.remoteClosers = append(hostif.remoteClosers, func() {
		log.Info("canceling host port control")
		cancelFn()
	})

	errCh := make(chan error)

	hostif.remotePortReq = func(msg *pktiopb.HostPortControlMessage) error {
		if err := srv.Send(msg); err != nil {
			errCh <- err
			return err
		}
		resp, err := srv.Recv()
		if err != nil {
			errCh <- err
			return err
		}
		return status.FromProto(resp.GetStatus()).Err()
	}

	// Send all existing hostif.
	for _, msg := range hostif.remoteHostifs {
		if err := hostif.remotePortReq(msg); err != nil {
			hostif.remoteMu.Unlock()
			return err
		}
	}
	hostif.remoteMu.Unlock()

	log.Info("initialized host port control channel")

	// The HostPortControls exits in two cases: context cancels or RPC errors.
	err = nil
	select {
	case <-ctx.Done():
		log.Info("host port control done")
	case err = <-errCh:
		log.Info("host port control err: %v", err)
	}

	hostif.remoteMu.Lock()
	hostif.remotePortReq = nil
	hostif.remoteMu.Unlock()
	log.Info("cleared host port control channel")
	return err
}
