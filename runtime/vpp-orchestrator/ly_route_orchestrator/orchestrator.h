#ifndef included_ly_route_orchestrator_h
#define included_ly_route_orchestrator_h

#include <vppinfra/lock.h>
#include <vnet/vnet.h>
#include <vnet/ip/ip.h>
#include <vnet/ethernet/ethernet.h>

#define LY_ORCH_NAME_LEN 64
#define LY_ORCH_GENERATION_LEN 80
#define LY_ORCH_MAX_HOPS 8
#define LY_ORCH_FLOW_SLOTS 8192
#define LY_ORCH_FLOW_PROBES 16
#define LY_ORCH_FLOW_TTL_SECONDS 300.0
#define LY_ORCH_MAC_LEN 6
#define LY_ORCH_MAC_ENTRIES 4096
#define LY_ORCH_MAC_PROBES 8
#define LY_ORCH_MAC_TTL_SECONDS 300.0
#define LY_ORCH_BYPASS_CAUSE_LEN 32
#define LY_ORCH_BYPASS_REASON_LEN 160
#define LY_ORCH_MAX_PROBE_TARGETS 64
#define LY_ORCH_INVALID_INDEX ((u32) ~0)

typedef enum
{
  LY_ORCH_ACTION_VIA,
  LY_ORCH_ACTION_DIRECT,
  LY_ORCH_ACTION_DROP,
} ly_orch_action_t;

typedef struct
{
  u8 name[LY_ORCH_NAME_LEN];
  u32 wan_sw_if_index;
  u32 lan_sw_if_index;
  u32 wan_next_index;
  u32 lan_next_index;
  u8 wan_next_hop_mac[LY_ORCH_MAC_LEN];
  u8 lan_next_hop_mac[LY_ORCH_MAC_LEN];
  u8 wan_next_hop_mac_valid;
  u8 lan_next_hop_mac_valid;
} ly_orch_group_t;

typedef struct
{
  u8 id[LY_ORCH_NAME_LEN];
  u32 group_position;
  u32 sequence;
  u32 target_group;
  u8 is_ip6;
  u8 protocol;
  u8 source_prefix_length;
  u8 destination_prefix_length;
  ip46_address_t source;
  ip46_address_t destination;
  u16 source_port_start;
  u16 source_port_end;
  u16 destination_port_start;
  u16 destination_port_end;
  ly_orch_action_t action;
} ly_orch_rule_t;

typedef struct
{
  u8 mac[LY_ORCH_MAC_LEN];
  u8 valid;
} ly_orch_if_mac_t;

typedef struct
{
  u32 wan_sw_if_index;
  u32 lan_sw_if_index;
  u32 wan_next_index;
  u32 lan_next_index;
  u8 wan_next_hop_mac[LY_ORCH_MAC_LEN];
  u8 lan_next_hop_mac[LY_ORCH_MAC_LEN];
  u8 wan_next_hop_mac_valid;
  u8 lan_next_hop_mac_valid;
  ly_orch_group_t *groups;
  ly_orch_rule_t *rules;
  /* Static next-hop MAC resolved per egress sw_if_index at commit time. */
  ly_orch_if_mac_t *egress_mac;
  ly_orch_action_t default_action;
  u8 generation[LY_ORCH_GENERATION_LEN];
  u8 valid;
} ly_orch_config_t;

typedef struct
{
  u64 packets;
  u64 bytes;
} ly_orch_rule_counter_t;

typedef struct
{
  ip46_address_t source;
  ip46_address_t destination;
  u16 source_port;
  u16 destination_port;
  u8 protocol;
  u8 is_ip6;
  u8 group_count;
  u8 occupied;
  u32 groups[LY_ORCH_MAX_HOPS];
  u64 packets;
  u64 bytes;
  f64 last_seen;
} ly_orch_flow_t;

/* Address-to-MAC cache.  The orchestrator is an L2 bump in the wire: every
 * forwarded frame must carry the next-hop MAC of the egress segment.  The
 * configured next-hop MAC is the deterministic fallback; addresses observed on
 * an interface override it so multi-host boundary segments (clients, an
 * upstream router rather than a single server) keep working. */
typedef struct
{
  u32 sw_if_index;
  ip46_address_t address;
  u8 mac[LY_ORCH_MAC_LEN];
  u8 is_ip6;
  u8 valid;
  u16 reserved;
  u32 hash;
  f64 last_seen;
} ly_orch_mac_entry_t;

/* Address-only view of the learned table.  A client attached to an
 * orchestration group port is reached through that port, so forwarding needs
 * "where did this address last appear" without knowing the egress up front.
 * Keeping it separate keeps the per-egress lookup (which also validates the
 * MAC) O(1). */
typedef struct
{
  ip46_address_t address;
  u8 mac[LY_ORCH_MAC_LEN];
  u8 is_ip6;
  u8 valid;
  u16 reserved;
  u32 sw_if_index;
  u32 hash;
  f64 last_seen;
} ly_orch_addr_entry_t;

typedef struct
{
  ly_orch_rule_counter_t *rule_counters;
  u64 *group_bypass_packets;
  u64 bypass_packets;
  ly_orch_flow_t *flows;
} ly_orch_worker_t;

/* One policy group's independent bypass flag.  The row is rebuilt on commit in
 * the same order as the active group vector so the packet path can index it by
 * group position; the name is kept for the CLI and readback. */
typedef struct
{
  u8 name[LY_ORCH_NAME_LEN];
  u8 active;
  u8 forced;
  u8 cause[LY_ORCH_BYPASS_CAUSE_LEN];
  u8 reason[LY_ORCH_BYPASS_REASON_LEN];
  f64 since;
} ly_orch_group_bypass_t;

/* Passive request/response analysis target.  A forward packet addressed to
 * destination:port is an attempt; the reverse packet (same five-tuple swapped)
 * is a response.  RST and ICMP unreachable are failure evidence. */
typedef struct
{
  u8 group[LY_ORCH_NAME_LEN];
  u32 group_position;
  ip46_address_t destination;
  u16 destination_port;
  u8 protocol;
  u8 is_ip6;
  u8 reserved[2];
  f64 window;
  u64 attempts;
  u64 syn;
  u64 synack;
  u64 rst;
  u64 responses;
  u64 icmp_unreachable;
} ly_orch_probe_target_t;

typedef struct
{
  vlib_main_t *vlib_main;
  vnet_main_t *vnet_main;
  ly_orch_config_t active;
  ly_orch_config_t candidate;
  ly_orch_worker_t *workers;
  u8 *enabled_by_sw_if_index;
  ly_orch_mac_entry_t *mac_entries;
  ly_orch_addr_entry_t *addr_entries;
  clib_spinlock_t mac_lock;
  /* Fault bypass.  When active the data path ignores every orchestration
   * policy rule (including drop/default drop) and forwards like a plain
   * transparent port pair, so a detected uplink/port fault cannot black-hole
   * the business flow.  The committed generation and candidate policy are
   * left untouched, so clearing the flag restores the configured policy
   * without a re-apply. */
  u8 bypass_active;
  u8 bypass_forced;
  u8 bypass_cause[LY_ORCH_BYPASS_CAUSE_LEN];
  u8 bypass_reason[LY_ORCH_BYPASS_REASON_LEN];
  f64 bypass_since;
  /* Per-group bypass rows, aligned with active.groups after commit. */
  ly_orch_group_bypass_t *group_bypass;
  /* Passive flow targets installed at runtime by the supervisor. */
  ly_orch_probe_target_t *probe_targets;
} ly_orch_main_t;

extern ly_orch_main_t ly_orch_main;
extern vlib_node_registration_t ly_orch_node;

void ly_orch_config_free (ly_orch_config_t *config);
u32 ly_orch_output_next (u32 sw_if_index);
void ly_orch_mac_table_init (void);
void ly_orch_mac_learn (u32 sw_if_index, const ip46_address_t *address,
                        u8 is_ip6, const u8 *mac, f64 now);
u8 ly_orch_mac_lookup (u32 sw_if_index, const ip46_address_t *address,
                       u8 is_ip6, u8 mac[LY_ORCH_MAC_LEN], f64 now);
u8 ly_orch_mac_lookup_address (const ip46_address_t *address, u8 is_ip6,
                               u32 *sw_if_index, u8 mac[LY_ORCH_MAC_LEN],
                               f64 now);

#endif
