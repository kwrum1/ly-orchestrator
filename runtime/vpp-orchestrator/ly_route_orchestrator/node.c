#include <vnet/feature/feature.h>
#include <vnet/bonding/node.h>
#include <vnet/tcp/tcp_packet.h>
#include <vnet/udp/udp_packet.h>
#include <ly_route_orchestrator/orchestrator.h>

typedef enum
{
  LY_ORCH_ERROR_FORWARDED,
  LY_ORCH_ERROR_DROPPED,
  LY_ORCH_ERROR_NON_IP,
  LY_ORCH_ERROR_NO_NEXT_HOP_MAC,
  LY_ORCH_ERROR_NOT_OURS,
  LY_ORCH_ERROR_FLOODED,
  LY_ORCH_ERROR_LOOPED,
  LY_ORCH_ERROR_BYPASS,
  LY_ORCH_N_ERROR,
} ly_orch_error_t;

static char *ly_orch_error_strings[] = {
  "packets transparently forwarded",
  "packets dropped by orchestration policy",
  "non-IP packets directly forwarded",
  "packets forwarded without an egress next-hop MAC",
  "unicast packets for another host ignored",
  "unicast packets flooded to learn the next-hop MAC",
  "own transmitted frames ignored (loop breaker)",
  "packets forwarded by fault bypass",
};

typedef struct
{
  ip46_address_t source;
  ip46_address_t destination;
  u16 source_port;
  u16 destination_port;
  u8 protocol;
  u8 is_ip6;
  u8 ports_valid;
  u8 valid;
} ly_orch_tuple_t;

typedef enum
{
  LY_ORCH_DIRECTION_FORWARD,
  LY_ORCH_DIRECTION_REVERSE,
  LY_ORCH_DIRECTION_INVALID,
} ly_orch_direction_t;

static_always_inline int
ly_orch_prefix_matches (const u8 *address, const u8 *prefix, u8 bits)
{
  u8 bytes = bits / 8;
  u8 remaining = bits % 8;
  if (bytes && memcmp (address, prefix, bytes) != 0)
    return 0;
  if (!remaining)
    return 1;
  u8 mask = (u8) (0xff << (8 - remaining));
  return (address[bytes] & mask) == (prefix[bytes] & mask);
}

static_always_inline ly_orch_tuple_t
ly_orch_parse_tuple (vlib_buffer_t *buffer)
{
  ly_orch_tuple_t tuple = { 0 };
  u8 *data = vlib_buffer_get_current (buffer);
  u32 length = buffer->current_length;
  u32 offset = sizeof (ethernet_header_t);
  u16 type;
  if (length < offset)
    return tuple;
  type = clib_net_to_host_u16 (*(u16 *) (data + 12));
  while ((type == ETHERNET_TYPE_VLAN || type == ETHERNET_TYPE_DOT1AD) &&
         length >= offset + 4)
    {
      type = clib_net_to_host_u16 (*(u16 *) (data + offset + 2));
      offset += 4;
    }
  if (type == ETHERNET_TYPE_IP4 && length >= offset + sizeof (ip4_header_t))
    {
      ip4_header_t *ip = (ip4_header_t *) (data + offset);
      u32 header_length = ip4_header_bytes (ip);
      if (header_length < sizeof (*ip) || length < offset + header_length)
        return tuple;
      tuple.source.ip4 = ip->src_address;
      tuple.destination.ip4 = ip->dst_address;
      tuple.protocol = ip->protocol;
      if ((ip->protocol == IP_PROTOCOL_TCP || ip->protocol == IP_PROTOCOL_UDP) &&
          length >= offset + header_length + 4)
        {
          udp_header_t *transport =
            (udp_header_t *) (data + offset + header_length);
          tuple.source_port = clib_net_to_host_u16 (transport->src_port);
          tuple.destination_port = clib_net_to_host_u16 (transport->dst_port);
          tuple.ports_valid = 1;
        }
      tuple.valid = 1;
      return tuple;
    }
  if (type == ETHERNET_TYPE_IP6 && length >= offset + sizeof (ip6_header_t))
    {
      ip6_header_t *ip = (ip6_header_t *) (data + offset);
      u32 transport_offset = offset + sizeof (*ip);
      u8 protocol = ip->protocol;
      u8 depth;
      tuple.is_ip6 = 1;
      tuple.source.ip6 = ip->src_address;
      tuple.destination.ip6 = ip->dst_address;
      for (depth = 0; depth < IP6_EXT_HDR_MAX; depth++)
        {
          u32 extension_length;
          ip6_ext_header_t *extension;
          if (ip6_ext_hdr (protocol))
            {
              if (length < transport_offset + sizeof (*extension))
                return tuple;
              extension = (ip6_ext_header_t *) (data + transport_offset);
              extension_length = ip6_ext_header_len (extension);
            }
          else if (protocol == IP_PROTOCOL_IPV6_FRAGMENTATION)
            {
              ip6_frag_hdr_t *fragment;
              if (length < transport_offset + sizeof (*fragment))
                return tuple;
              fragment = (ip6_frag_hdr_t *) (data + transport_offset);
              protocol = fragment->next_hdr;
              transport_offset += sizeof (*fragment);
              if (ip6_frag_hdr_offset (fragment) > 0)
                break;
              continue;
            }
          else if (protocol == IP_PROTOCOL_IPSEC_AH)
            {
              if (length < transport_offset + sizeof (*extension))
                return tuple;
              extension = (ip6_ext_header_t *) (data + transport_offset);
              extension_length = ip6_ext_authhdr_len (extension);
            }
          else
            break;
          if (extension_length < sizeof (*extension) ||
              length < transport_offset + extension_length)
            return tuple;
          protocol = extension->next_hdr;
          transport_offset += extension_length;
        }
      tuple.protocol = protocol;
      if ((protocol == IP_PROTOCOL_TCP || protocol == IP_PROTOCOL_UDP) &&
          length >= transport_offset + 4)
        {
          udp_header_t *transport = (udp_header_t *) (data + transport_offset);
          tuple.source_port = clib_net_to_host_u16 (transport->src_port);
          tuple.destination_port = clib_net_to_host_u16 (transport->dst_port);
          tuple.ports_valid = 1;
        }
      tuple.valid = 1;
    }
  return tuple;
}

static_always_inline void
ly_orch_reverse_tuple (ly_orch_tuple_t *tuple)
{
  ip46_address_t address = tuple->source;
  u16 port = tuple->source_port;
  tuple->source = tuple->destination;
  tuple->destination = address;
  tuple->source_port = tuple->destination_port;
  tuple->destination_port = port;
}

static_always_inline int
ly_orch_rule_matches (const ly_orch_rule_t *rule,
                      const ly_orch_tuple_t *tuple)
{
  const u8 *source;
  const u8 *destination;
  const u8 *rule_source;
  const u8 *rule_destination;
  if (!tuple->valid || rule->is_ip6 != tuple->is_ip6 ||
      (rule->protocol && rule->protocol != tuple->protocol))
    return 0;
  source = tuple->is_ip6 ? tuple->source.ip6.as_u8 : tuple->source.ip4.as_u8;
  destination = tuple->is_ip6 ? tuple->destination.ip6.as_u8 :
                                tuple->destination.ip4.as_u8;
  rule_source = rule->is_ip6 ? rule->source.ip6.as_u8 : rule->source.ip4.as_u8;
  rule_destination = rule->is_ip6 ? rule->destination.ip6.as_u8 :
                                    rule->destination.ip4.as_u8;
  if (!ly_orch_prefix_matches (source, rule_source,
                               rule->source_prefix_length) ||
      !ly_orch_prefix_matches (destination, rule_destination,
                               rule->destination_prefix_length))
    return 0;
  if (rule->protocol == IP_PROTOCOL_TCP || rule->protocol == IP_PROTOCOL_UDP)
    return (!tuple->ports_valid && rule->source_port_start == 0 &&
            rule->source_port_end == 65535 &&
            rule->destination_port_start == 0 &&
            rule->destination_port_end == 65535) ||
           (tuple->ports_valid &&
            tuple->source_port >= rule->source_port_start &&
           tuple->source_port <= rule->source_port_end &&
           tuple->destination_port >= rule->destination_port_start &&
            tuple->destination_port <= rule->destination_port_end);
  return 1;
}

static_always_inline int
ly_orch_group_available (ly_orch_config_t *config, u32 group_index)
{
  ly_orch_group_t *group = &config->groups[group_index];
  vnet_main_t *vnm = ly_orch_main.vnet_main;
  return vnet_sw_interface_is_up (vnm, group->wan_sw_if_index) &&
         vnet_sw_interface_is_up (vnm, group->lan_sw_if_index);
}

static_always_inline u8
ly_orch_build_path (ly_orch_config_t *config, const ly_orch_tuple_t *tuple,
                    u32 path[LY_ORCH_MAX_HOPS], ly_orch_worker_t *worker,
                    u32 packet_bytes, u8 *drop)
{
  u32 index = 0;
  u8 path_count = 0;
  *drop = 0;
  while (index < vec_len (config->rules))
    {
      u32 group_position = config->rules[index].group_position;
      u32 scan = index;
      int selected = -1;
      while (scan < vec_len (config->rules) &&
             config->rules[scan].group_position == group_position)
        {
          if (selected < 0 && ly_orch_rule_matches (&config->rules[scan], tuple))
            selected = scan;
          scan++;
        }
      if (selected >= 0)
        {
          ly_orch_rule_t *rule = &config->rules[selected];
          if (worker)
            {
              worker->rule_counters[selected].packets++;
              worker->rule_counters[selected].bytes += packet_bytes;
            }
          if (rule->action == LY_ORCH_ACTION_DROP)
            {
              *drop = 1;
              return path_count;
            }
          if (rule->action == LY_ORCH_ACTION_VIA)
            {
              if (!ly_orch_group_available (config, rule->target_group))
                {
                  if (worker)
                    worker->group_bypass_packets[rule->target_group]++;
                  /* First matching group is terminal even when bypassed. */
                  return path_count;
                }
              if (path_count >= LY_ORCH_MAX_HOPS)
                {
                  *drop = 1;
                  return path_count;
                }
              path[path_count++] = rule->target_group;
            }
          /* First matching group decides: direct and via terminate here just
           * like drop. Only a group with no matching rule falls through. */
          return path_count;
        }
      index = scan;
    }
  /* The default action only applies to traffic no rule matched. */
  if (config->default_action == LY_ORCH_ACTION_DROP)
    *drop = 1;
  return path_count;
}

/* Which side of the deployment did the frame enter from?
 *
 *   LAN boundary and every group lan_facing port face clients;
 *   WAN boundary and every group wan_facing port face servers.
 *
 * Policy rules are authored client-to-server, so only server-side ingress has
 * to be reversed before matching.  Treating group ports as opaque "service
 * return" ports (the previous behaviour) inverted the tuple on the client side
 * and made port rules evaluate the wrong direction.
 */
typedef struct
{
  u8 client_side;
  u32 group_index;
} ly_orch_ingress_t;

static_always_inline ly_orch_ingress_t
ly_orch_classify_ingress (ly_orch_config_t *config, u32 rx)
{
  ly_orch_ingress_t ingress = { .client_side = 1,
                                .group_index = LY_ORCH_INVALID_INDEX };
  u32 index;
  if (rx == config->wan_sw_if_index)
    {
      ingress.client_side = 0;
      return ingress;
    }
  if (rx == config->lan_sw_if_index)
    return ingress;
  vec_foreach_index (index, config->groups)
    {
      if (rx == config->groups[index].lan_sw_if_index)
        {
          ingress.group_index = index;
          return ingress;
        }
      if (rx == config->groups[index].wan_sw_if_index)
        {
          ingress.group_index = index;
          ingress.client_side = 0;
          return ingress;
        }
    }
  /* Not a configured port: the node is only enabled on configured ports, so
   * this is a stale feature flag.  Keep the old drop behaviour. */
  ingress.group_index = LY_ORCH_INVALID_INDEX;
  return ingress;
}

static_always_inline u32
ly_orch_logical_ingress (u32 rx)
{
  member_if_t *member = bond_get_member_by_sw_if_index (rx);
  bond_if_t *bond;

  if (!member)
    return rx;
  bond = bond_get_bond_if_by_dev_instance (member->bif_dev_instance);
  return bond ? bond->sw_if_index : rx;
}

/* Resolve the next-hop MAC used when a frame leaves through egress.
 *
 * A configured per-egress binding is the operator contract for that segment
 * and always wins: a frame observed on the segment must never be able to
 * hijack the egress MAC (a stray/spoofed address used to redirect traffic to
 * another host). Segments without a binding keep using the learned table so
 * multi-host boundaries still work. */
static_always_inline u8
ly_orch_resolve_next_hop_mac (ly_orch_config_t *config, u32 egress,
                              const ip46_address_t *destination, u8 is_ip6,
                              u8 mac[LY_ORCH_MAC_LEN], f64 now)
{
  if (egress < vec_len (config->egress_mac) && config->egress_mac[egress].valid)
    {
      clib_memcpy_fast (mac, config->egress_mac[egress].mac, LY_ORCH_MAC_LEN);
      return 1;
    }
  if (ly_orch_mac_lookup (egress, destination, is_ip6, mac, now))
    return 1;
  return 0;
}

/* Frames emitted by one of our own ports can come back on a sibling port when
 * two group ports share an L2 segment (the lab chains groups over vlan10).
 * Forwarding them again creates a line-rate loop, so our own source MAC is
 * always ignored.  This is also the loop breaker for flooded unknown-unicast
 * frames. */
static_always_inline int
ly_orch_port_mac_matches (u32 sw_if_index, const u8 *source)
{
  vnet_hw_interface_t *hw;
  if (sw_if_index == LY_ORCH_INVALID_INDEX)
    return 0;
  hw = vnet_get_sup_hw_interface (ly_orch_main.vnet_main, sw_if_index);
  return hw && vec_len (hw->hw_address) >= LY_ORCH_MAC_LEN &&
         memcmp (source, hw->hw_address, LY_ORCH_MAC_LEN) == 0;
}

static_always_inline int
ly_orch_frame_from_self (ly_orch_config_t *config, const u8 *source)
{
  u32 index;
  if (ly_orch_port_mac_matches (config->wan_sw_if_index, source) ||
      ly_orch_port_mac_matches (config->lan_sw_if_index, source))
    return 1;
  vec_foreach_index (index, config->groups)
    if (ly_orch_port_mac_matches (config->groups[index].wan_sw_if_index,
                                  source) ||
        ly_orch_port_mac_matches (config->groups[index].lan_sw_if_index,
                                  source))
      return 1;
  return 0;
}

/* Learn the source address of a frame only when the frame was actually
 * delivered to this interface (unicast to our MAC) or was a broadcast.  The
 * host interface runs promiscuously, so without this filter every neighbour
 * on the segment could poison the address table. */
static_always_inline int
ly_orch_frame_is_ours (u32 sw_if_index, const ethernet_header_t *ethernet)
{
  vnet_hw_interface_t *hw;
  if (ethernet->dst_address[0] & 1)
    return 1;
  hw = vnet_get_sup_hw_interface (ly_orch_main.vnet_main, sw_if_index);
  if (!hw || vec_len (hw->hw_address) < LY_ORCH_MAC_LEN)
    return 0;
  return memcmp (ethernet->dst_address, hw->hw_address, LY_ORCH_MAC_LEN) == 0;
}

/* Rewrite the L2 header for a transparent L3 hop: destination becomes the
 * next-hop MAC (when known), source always becomes the egress interface MAC.
 * The IP header (and therefore TTL, checksums and payload) is left untouched.
 * VLAN tags live after the 14 byte ethernet header, so rewriting bytes 0..11
 * stays valid for tagged and untagged frames. */
static_always_inline int
ly_orch_rewrite_ethernet (vlib_main_t *vm, vlib_buffer_t *buffer, u32 egress,
                          const u8 next_hop_mac[LY_ORCH_MAC_LEN],
                          u8 set_destination)
{
  ethernet_header_t *ethernet;
  vnet_hw_interface_t *hw;
  if (buffer->current_length < sizeof (ethernet_header_t))
    return 0;
  ethernet = vlib_buffer_get_current (buffer);
  hw = vnet_get_sup_hw_interface (ly_orch_main.vnet_main, egress);
  if (!hw || vec_len (hw->hw_address) < LY_ORCH_MAC_LEN)
    return 0;
  if (set_destination)
    clib_memcpy_fast (ethernet->dst_address, next_hop_mac, LY_ORCH_MAC_LEN);
  clib_memcpy_fast (ethernet->src_address, hw->hw_address,
                    LY_ORCH_MAC_LEN);
  return 1;
}

static_always_inline u64
ly_orch_flow_hash (const ly_orch_tuple_t *tuple)
{
  return hash_memory ((void *) tuple, sizeof (*tuple), 0x6c796f72);
}

static_always_inline int
ly_orch_flow_equal (const ly_orch_flow_t *flow, const ly_orch_tuple_t *tuple)
{
  if (!flow->occupied || flow->is_ip6 != tuple->is_ip6 ||
      flow->protocol != tuple->protocol ||
      flow->source_port != tuple->source_port ||
      flow->destination_port != tuple->destination_port)
    return 0;
  if (tuple->is_ip6)
    return ip6_address_is_equal (&flow->source.ip6, &tuple->source.ip6) &&
           ip6_address_is_equal (&flow->destination.ip6,
                                 &tuple->destination.ip6);
  return ip4_address_is_equal (&flow->source.ip4, &tuple->source.ip4) &&
         ip4_address_is_equal (&flow->destination.ip4,
                               &tuple->destination.ip4);
}

static_always_inline void
ly_orch_record_flow (vlib_main_t *vm, ly_orch_worker_t *worker,
                     const ly_orch_tuple_t *tuple, const u32 *path,
                     u8 path_count, u32 packet_bytes)
{
  u32 start = ly_orch_flow_hash (tuple) & (LY_ORCH_FLOW_SLOTS - 1);
  u32 probe, selected = start;
  f64 oldest = 1e30;
  f64 now = vlib_time_now (vm);
  ly_orch_flow_t *flow;
  for (probe = 0; probe < LY_ORCH_FLOW_PROBES; probe++)
    {
      u32 slot = (start + probe) & (LY_ORCH_FLOW_SLOTS - 1);
      flow = &worker->flows[slot];
      if (!flow->occupied || now - flow->last_seen > LY_ORCH_FLOW_TTL_SECONDS ||
          ly_orch_flow_equal (flow, tuple))
        {
          selected = slot;
          break;
        }
      if (flow->last_seen < oldest)
        {
          oldest = flow->last_seen;
          selected = slot;
        }
    }
  flow = &worker->flows[selected];
  if (!ly_orch_flow_equal (flow, tuple))
    {
      clib_memset (flow, 0, sizeof (*flow));
      flow->source = tuple->source;
      flow->destination = tuple->destination;
      flow->source_port = tuple->source_port;
      flow->destination_port = tuple->destination_port;
      flow->protocol = tuple->protocol;
      flow->is_ip6 = tuple->is_ip6;
      flow->group_count = path_count;
      clib_memcpy_fast (flow->groups, path, sizeof (u32) * path_count);
      flow->occupied = 1;
    }
  flow->packets++;
  flow->bytes += packet_bytes;
  flow->last_seen = now;
}

/* Next-node index for one of the configured ports.  All ports are resolved at
 * commit time; the fallback only runs for an interface learned outside the
 * configured set (which cannot happen today) and keeps the data path from
 * calling vlib_node_add_next for every packet. */
static_always_inline u32
ly_orch_next_for (ly_orch_config_t *config, u32 sw_if_index)
{
  u32 index;
  if (sw_if_index == config->wan_sw_if_index)
    return config->wan_next_index;
  if (sw_if_index == config->lan_sw_if_index)
    return config->lan_next_index;
  vec_foreach_index (index, config->groups)
    {
      if (sw_if_index == config->groups[index].wan_sw_if_index)
        return config->groups[index].wan_next_index;
      if (sw_if_index == config->groups[index].lan_sw_if_index)
        return config->groups[index].lan_next_index;
    }
  return ly_orch_output_next (sw_if_index);
}

/* Pick the egress interface.
 *
 * A policy path (via) is authoritative and is applied logically inside VPP:
 * client-to-server traffic leaves the WAN-facing port of the last group in
 * the path, server-to-client traffic leaves the LAN-facing port of the first
 * group.  Without a path the destination address decides: the address-to-port
 * table learned from real frames lets a client attached to a group port be
 * reached through that same port in the return direction (a client on
 * grp-b/ens193 is answered through ens193, not through the LAN boundary).
 * If the address is unknown the frame leaves the opposite port of the ingress
 * group so a group port behaves like a real transparent port pair.
 */
static_always_inline u16
ly_orch_target_next (ly_orch_config_t *config, ly_orch_ingress_t ingress,
                     u32 rx, const u32 *path, u8 path_count,
                     const ly_orch_tuple_t *tuple, f64 now,
                     u32 *target_sw_if_index)
{
  u32 target = LY_ORCH_INVALID_INDEX;
  u8 mac[LY_ORCH_MAC_LEN];
  u32 learned_if = LY_ORCH_INVALID_INDEX;

  if (path_count)
    {
      u32 group = ingress.client_side ? path[path_count - 1] : path[0];
      if (ingress.client_side)
        {
          target = config->groups[group].wan_sw_if_index;
          *target_sw_if_index = target;
          return config->groups[group].wan_next_index;
        }
      target = config->groups[group].lan_sw_if_index;
      *target_sw_if_index = target;
      return config->groups[group].lan_next_index;
    }
  if (tuple->valid &&
      ly_orch_mac_lookup_address (&tuple->destination, tuple->is_ip6,
                                  &learned_if, mac, now) &&
      learned_if != rx)
    target = learned_if;
  if (target == LY_ORCH_INVALID_INDEX)
    {
      if (ingress.group_index != LY_ORCH_INVALID_INDEX)
        {
          /* A group port is a transparent port pair: a client entering the
           * lan_facing side leaves the wan_facing side and vice versa.  The
           * learned-destination lookup above already handles clients behind
           * another group port. */
          ly_orch_group_t *group = &config->groups[ingress.group_index];
          target = ingress.client_side ? group->wan_sw_if_index :
                                         group->lan_sw_if_index;
        }
      else
        target = ingress.client_side ? config->wan_sw_if_index :
                                       config->lan_sw_if_index;
    }
  *target_sw_if_index = target;
  return ly_orch_next_for (config, target);
}

VLIB_NODE_FN (ly_orch_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
                             vlib_frame_t *frame)
{
  ly_orch_main_t *om = &ly_orch_main;
  ly_orch_config_t *config = &om->active;
  ly_orch_worker_t *worker = &om->workers[vm->thread_index];
  u32 *from = vlib_frame_vector_args (frame);
  u32 buffers[VLIB_FRAME_SIZE];
  u16 nexts[VLIB_FRAME_SIZE];
  u32 forwarded = 0, dropped = 0, non_ip = 0, no_next_hop_mac = 0;
  u32 not_ours = 0, flooded = 0, looped = 0, bypassed = 0, index;

  for (index = 0; index < frame->n_vectors; index++)
    {
      u32 buffer_index = from[index];
      vlib_buffer_t *buffer = vlib_get_buffer (vm, buffer_index);
      u32 rx = ly_orch_logical_ingress (
        vnet_buffer (buffer)->sw_if_index[VLIB_RX]);
      u32 raw_rx = vnet_buffer (buffer)->sw_if_index[VLIB_RX];
      u32 target = LY_ORCH_INVALID_INDEX;
      u32 path[LY_ORCH_MAX_HOPS] = { 0 };
      u8 drop = 0;
      ly_orch_tuple_t tuple = ly_orch_parse_tuple (buffer);
      /* The policy matcher may reverse the tuple; keep the on-wire
       * orientation for L2 next-hop resolution and MAC learning. */
      ly_orch_tuple_t wire_tuple = tuple;
      ly_orch_ingress_t ingress = ly_orch_classify_ingress (config, rx);
      u16 next = 0;
      u8 path_count;
      u32 packet_bytes = vlib_buffer_length_in_chain (vm, buffer);
      f64 now = vlib_time_now (vm);

      if (buffer->current_length >= sizeof (ethernet_header_t))
        {
          ethernet_header_t *ethernet = vlib_buffer_get_current (buffer);
          if (ly_orch_frame_from_self (config, ethernet->src_address))
            {
              buffer->error = node->errors[LY_ORCH_ERROR_LOOPED];
              nexts[index] = 0;
              looped++;
              buffers[index] = buffer_index;
              continue;
            }
          if (!ly_orch_frame_is_ours (raw_rx, ethernet))
            {
              /* A unicast frame addressed to another host on the segment is
               * delivered there by the switch.  The host interface runs
               * promiscuously, so without this check the orchestrator would
               * steal and reroute its neighbours' traffic. */
              buffer->error = node->errors[LY_ORCH_ERROR_NOT_OURS];
              nexts[index] = 0;
              not_ours++;
              buffers[index] = buffer_index;
              continue;
            }
          if (wire_tuple.valid)
            ly_orch_mac_learn (raw_rx, &wire_tuple.source, wire_tuple.is_ip6,
                               ethernet->src_address, now);
        }

      if (!config->valid)
        drop = 1;
      /* Policy rules are authored client-to-server.  Only server-side
       * ingress (WAN boundary or a group wan_facing port) carries the return
       * orientation and has to be reversed before matching. */
      if (!ingress.client_side && tuple.valid)
        ly_orch_reverse_tuple (&tuple);
      if (!drop && om->bypass_active)
        {
          /* Fault bypass: do not evaluate any rule (including drop/default
           * drop).  The packet is forwarded through the plain transparent
           * port-pair path below, exactly like a configuration with no
           * matching policy. */
          path_count = 0;
          if (worker)
            worker->bypass_packets++;
          bypassed++;
        }
      else if (!drop && tuple.valid)
        path_count = ly_orch_build_path (config, &tuple, path, worker,
                                         packet_bytes, &drop);
      else
        path_count = 0;
      if (!drop)
        next = ly_orch_target_next (config, ingress, rx, path, path_count,
                                    &wire_tuple, now, &target);
      if (!next || target == LY_ORCH_INVALID_INDEX)
        drop = 1;

      if (tuple.valid)
        ly_orch_record_flow (vm, worker, &tuple, path, path_count,
                             packet_bytes);

      if (drop)
        {
          buffer->error = node->errors[LY_ORCH_ERROR_DROPPED];
          nexts[index] = 0;
          dropped++;
        }
      else
        {
          if (buffer->current_length >= sizeof (ethernet_header_t))
            {
              ethernet_header_t *ethernet = vlib_buffer_get_current (buffer);
              u8 multicast = ethernet->dst_address[0] & 1;
              u8 next_hop_mac[LY_ORCH_MAC_LEN];
              u8 set_destination = 0;

              if (!multicast)
                {
                  if (ly_orch_resolve_next_hop_mac (config, target,
                                                    &wire_tuple.destination,
                                                    wire_tuple.is_ip6,
                                                    next_hop_mac, now))
                    set_destination = 1;
                  else
                    {
                      /* Transparent-bridge behaviour: an unknown unicast
                       * destination is flooded once so the host answers and
                       * the address table learns its MAC.  Never silently
                       * forward a frame with an unusable destination MAC. */
                      clib_memset (next_hop_mac, 0xff, LY_ORCH_MAC_LEN);
                      set_destination = 1;
                      flooded++;
                    }
                }
              if (!ly_orch_rewrite_ethernet (vm, buffer, target, next_hop_mac,
                                             set_destination))
                no_next_hop_mac++;
            }
          vnet_buffer (buffer)->sw_if_index[VLIB_TX] = target;
          nexts[index] = next;
          forwarded++;
          if (!tuple.valid)
            non_ip++;
        }
      buffers[index] = buffer_index;
    }
  vlib_buffer_enqueue_to_next (vm, node, buffers, nexts, frame->n_vectors);
  vlib_node_increment_counter (vm, node->node_index, LY_ORCH_ERROR_FORWARDED,
                               forwarded);
  vlib_node_increment_counter (vm, node->node_index, LY_ORCH_ERROR_DROPPED,
                               dropped);
  vlib_node_increment_counter (vm, node->node_index, LY_ORCH_ERROR_NON_IP,
                               non_ip);
  vlib_node_increment_counter (vm, node->node_index,
                               LY_ORCH_ERROR_NO_NEXT_HOP_MAC,
                               no_next_hop_mac);
  vlib_node_increment_counter (vm, node->node_index, LY_ORCH_ERROR_NOT_OURS,
                               not_ours);
  vlib_node_increment_counter (vm, node->node_index, LY_ORCH_ERROR_FLOODED,
                               flooded);
  vlib_node_increment_counter (vm, node->node_index, LY_ORCH_ERROR_LOOPED,
                               looped);
  vlib_node_increment_counter (vm, node->node_index, LY_ORCH_ERROR_BYPASS,
                               bypassed);
  return frame->n_vectors;
}

VLIB_REGISTER_NODE (ly_orch_node) = {
  .name = "ly-route-orchestrator",
  .vector_size = sizeof (u32),
  .type = VLIB_NODE_TYPE_INTERNAL,
  .n_errors = LY_ORCH_N_ERROR,
  .error_strings = ly_orch_error_strings,
  .n_next_nodes = 1,
  .next_nodes = { [0] = "error-drop" },
};
