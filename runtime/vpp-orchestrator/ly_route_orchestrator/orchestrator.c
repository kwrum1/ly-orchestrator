#include <vnet/feature/feature.h>
#include <vnet/bonding/node.h>
#include <vnet/plugin/plugin.h>
#include <vpp/app/version.h>
#include <ly_route_orchestrator/orchestrator.h>

ly_orch_main_t ly_orch_main;

VLIB_PLUGIN_REGISTER () = {
  .version = VPP_BUILD_VER,
  .description = "LY-Route transparent traffic orchestrator",
};

static void
ly_orch_copy_name (u8 destination[LY_ORCH_NAME_LEN], const u8 *source)
{
  clib_memset (destination, 0, LY_ORCH_NAME_LEN);
  if (source)
    clib_strncpy ((char *) destination, (const char *) source,
                  LY_ORCH_NAME_LEN - 1);
}

void
ly_orch_config_free (ly_orch_config_t *config)
{
  vec_free (config->groups);
  vec_free (config->rules);
  vec_free (config->egress_mac);
  clib_memset (config, 0, sizeof (*config));
  config->wan_sw_if_index = LY_ORCH_INVALID_INDEX;
  config->lan_sw_if_index = LY_ORCH_INVALID_INDEX;
  config->wan_next_index = LY_ORCH_INVALID_INDEX;
  config->lan_next_index = LY_ORCH_INVALID_INDEX;
}

static inline u32
ly_orch_mac_hash (u32 sw_if_index, const ip46_address_t *address, u8 is_ip6)
{
  u8 key[sizeof (u32) + sizeof (ip46_address_t) + 1];
  u32 length = is_ip6 ? sizeof (ip6_address_t) : sizeof (ip4_address_t);
  clib_memcpy_fast (key, &sw_if_index, sizeof (u32));
  clib_memcpy_fast (key + sizeof (u32), address, length);
  key[sizeof (u32) + length] = is_ip6;
  return hash_memory (key, sizeof (u32) + length + 1, 0x6c796d63);
}

static inline int
ly_orch_mac_entry_matches (const ly_orch_mac_entry_t *entry,
                           u32 sw_if_index, const ip46_address_t *address,
                           u8 is_ip6)
{
  u32 length = is_ip6 ? sizeof (ip6_address_t) : sizeof (ip4_address_t);
  return entry->valid && entry->sw_if_index == sw_if_index &&
         entry->is_ip6 == is_ip6 &&
         memcmp (&entry->address, address, length) == 0;
}

void
ly_orch_mac_table_init (void)
{
  ly_orch_main_t *om = &ly_orch_main;
  if (om->mac_entries)
    return;
  vec_validate (om->mac_entries, LY_ORCH_MAC_ENTRIES - 1);
  clib_memset (om->mac_entries, 0,
               sizeof (ly_orch_mac_entry_t) * LY_ORCH_MAC_ENTRIES);
  vec_validate (om->addr_entries, LY_ORCH_MAC_ENTRIES - 1);
  clib_memset (om->addr_entries, 0,
               sizeof (ly_orch_addr_entry_t) * LY_ORCH_MAC_ENTRIES);
  clib_spinlock_init (&om->mac_lock);
}

static inline u32
ly_orch_addr_hash (const ip46_address_t *address, u8 is_ip6)
{
  u8 key[sizeof (ip46_address_t) + 1];
  u32 length = is_ip6 ? sizeof (ip6_address_t) : sizeof (ip4_address_t);
  clib_memcpy_fast (key, address, length);
  key[length] = is_ip6;
  return hash_memory (key, length + 1, 0x6c796164);
}

static inline int
ly_orch_addr_entry_matches (const ly_orch_addr_entry_t *entry,
                            const ip46_address_t *address, u8 is_ip6)
{
  u32 length = is_ip6 ? sizeof (ip6_address_t) : sizeof (ip4_address_t);
  return entry->valid && entry->is_ip6 == is_ip6 &&
         memcmp (&entry->address, address, length) == 0;
}

/* Record where an address was last seen.  The newest sighting wins so a
 * client that moves between a boundary port and a group port converges on the
 * port carrying its current traffic. */
static void
ly_orch_addr_learn_locked (const ip46_address_t *address, u8 is_ip6,
                           u32 sw_if_index, const u8 *mac, f64 now)
{
  ly_orch_main_t *om = &ly_orch_main;
  u32 start, probe, selected = LY_ORCH_INVALID_INDEX;
  u32 empty = LY_ORCH_INVALID_INDEX, oldest_slot = LY_ORCH_INVALID_INDEX;
  f64 oldest = 1e30;
  u32 hash = ly_orch_addr_hash (address, is_ip6);

  start = hash & (LY_ORCH_MAC_ENTRIES - 1);
  for (probe = 0; probe < LY_ORCH_MAC_PROBES; probe++)
    {
      u32 slot = (start + probe) & (LY_ORCH_MAC_ENTRIES - 1);
      ly_orch_addr_entry_t *entry = &om->addr_entries[slot];
      if (ly_orch_addr_entry_matches (entry, address, is_ip6))
        {
          selected = slot;
          break;
        }
      if (!entry->valid && empty == LY_ORCH_INVALID_INDEX)
        empty = slot;
      if (entry->valid && entry->last_seen < oldest)
        {
          oldest = entry->last_seen;
          oldest_slot = slot;
        }
    }
  if (selected == LY_ORCH_INVALID_INDEX)
    selected = empty != LY_ORCH_INVALID_INDEX ? empty : oldest_slot;
  if (selected == LY_ORCH_INVALID_INDEX)
    return;
  {
    ly_orch_addr_entry_t *entry = &om->addr_entries[selected];
    if (!ly_orch_addr_entry_matches (entry, address, is_ip6))
      {
        clib_memset (entry, 0, sizeof (*entry));
        entry->address = *address;
        entry->is_ip6 = is_ip6;
        entry->hash = hash;
        entry->valid = 1;
      }
    entry->sw_if_index = sw_if_index;
    clib_memcpy_fast (entry->mac, mac, LY_ORCH_MAC_LEN);
    entry->last_seen = now;
  }
}

void
ly_orch_mac_learn (u32 sw_if_index, const ip46_address_t *address,
                   u8 is_ip6, const u8 *mac, f64 now)
{
  ly_orch_main_t *om = &ly_orch_main;
  u32 start, probe, selected = LY_ORCH_INVALID_INDEX;
  u32 empty = LY_ORCH_INVALID_INDEX, oldest_slot = LY_ORCH_INVALID_INDEX;
  f64 oldest = 1e30;
  u32 hash;

  if (!om->mac_entries || !mac)
    return;
  hash = ly_orch_mac_hash (sw_if_index, address, is_ip6);
  start = hash & (LY_ORCH_MAC_ENTRIES - 1);
  clib_spinlock_lock (&om->mac_lock);
  for (probe = 0; probe < LY_ORCH_MAC_PROBES; probe++)
    {
      u32 slot = (start + probe) & (LY_ORCH_MAC_ENTRIES - 1);
      ly_orch_mac_entry_t *entry = &om->mac_entries[slot];
      if (ly_orch_mac_entry_matches (entry, sw_if_index, address, is_ip6))
        {
          selected = slot;
          break;
        }
      if (!entry->valid && empty == LY_ORCH_INVALID_INDEX)
        empty = slot;
      if (entry->valid && entry->last_seen < oldest)
        {
          oldest = entry->last_seen;
          oldest_slot = slot;
        }
    }
  if (selected == LY_ORCH_INVALID_INDEX)
    selected = empty != LY_ORCH_INVALID_INDEX ? empty : oldest_slot;
  if (selected != LY_ORCH_INVALID_INDEX)
    {
      ly_orch_mac_entry_t *entry = &om->mac_entries[selected];
      if (!ly_orch_mac_entry_matches (entry, sw_if_index, address, is_ip6))
        {
          clib_memset (entry, 0, sizeof (*entry));
          entry->sw_if_index = sw_if_index;
          entry->address = *address;
          entry->is_ip6 = is_ip6;
          entry->hash = hash;
          entry->valid = 1;
        }
      clib_memcpy_fast (entry->mac, mac, LY_ORCH_MAC_LEN);
      entry->last_seen = now;
    }
  ly_orch_addr_learn_locked (address, is_ip6, sw_if_index, mac, now);
  clib_spinlock_unlock (&om->mac_lock);
}

u8
ly_orch_mac_lookup (u32 sw_if_index, const ip46_address_t *address, u8 is_ip6,
                    u8 mac[LY_ORCH_MAC_LEN], f64 now)
{
  ly_orch_main_t *om = &ly_orch_main;
  u32 start, probe;
  u8 found = 0;

  if (!om->mac_entries)
    return 0;
  start = ly_orch_mac_hash (sw_if_index, address, is_ip6) &
          (LY_ORCH_MAC_ENTRIES - 1);
  clib_spinlock_lock (&om->mac_lock);
  for (probe = 0; probe < LY_ORCH_MAC_PROBES; probe++)
    {
      u32 slot = (start + probe) & (LY_ORCH_MAC_ENTRIES - 1);
      ly_orch_mac_entry_t *entry = &om->mac_entries[slot];
      if (!entry->valid)
        break;
      if (!ly_orch_mac_entry_matches (entry, sw_if_index, address, is_ip6))
        continue;
      if (now - entry->last_seen <= LY_ORCH_MAC_TTL_SECONDS)
        {
          clib_memcpy_fast (mac, entry->mac, LY_ORCH_MAC_LEN);
          found = 1;
        }
      break;
    }
  clib_spinlock_unlock (&om->mac_lock);
  return found;
}

/* Where did this address last appear?  Used to reach clients attached to an
 * orchestration group port instead of always leaving through a boundary. */
u8
ly_orch_mac_lookup_address (const ip46_address_t *address, u8 is_ip6,
                            u32 *sw_if_index, u8 mac[LY_ORCH_MAC_LEN], f64 now)
{
  ly_orch_main_t *om = &ly_orch_main;
  u32 start, probe;
  u8 found = 0;

  if (!om->addr_entries)
    return 0;
  start = ly_orch_addr_hash (address, is_ip6) & (LY_ORCH_MAC_ENTRIES - 1);
  clib_spinlock_lock (&om->mac_lock);
  for (probe = 0; probe < LY_ORCH_MAC_PROBES; probe++)
    {
      u32 slot = (start + probe) & (LY_ORCH_MAC_ENTRIES - 1);
      ly_orch_addr_entry_t *entry = &om->addr_entries[slot];
      if (!ly_orch_addr_entry_matches (entry, address, is_ip6))
        continue;
      if (now - entry->last_seen <= LY_ORCH_MAC_TTL_SECONDS)
        {
          clib_memcpy_fast (mac, entry->mac, LY_ORCH_MAC_LEN);
          *sw_if_index = entry->sw_if_index;
          found = 1;
        }
      break;
    }
  clib_spinlock_unlock (&om->mac_lock);
  return found;
}

/* Resolve the static next-hop MAC that applies to one egress interface. */
static void
ly_orch_config_set_egress_mac (ly_orch_config_t *config, u32 sw_if_index,
                               const u8 *mac, u8 mac_valid)
{
  u32 index, old_len;
  if (sw_if_index == LY_ORCH_INVALID_INDEX)
    return;
  old_len = vec_len (config->egress_mac);
  if (old_len <= sw_if_index)
    {
      vec_validate (config->egress_mac, sw_if_index);
      for (index = old_len; index <= sw_if_index; index++)
        clib_memset (&config->egress_mac[index], 0,
                     sizeof (ly_orch_if_mac_t));
    }
  if (!mac_valid)
    {
      config->egress_mac[sw_if_index].valid = 0;
      return;
    }
  clib_memcpy_fast (config->egress_mac[sw_if_index].mac, mac, LY_ORCH_MAC_LEN);
  config->egress_mac[sw_if_index].valid = 1;
}

static void
ly_orch_config_build_egress_macs (ly_orch_config_t *config)
{
  u32 index;
  ly_orch_config_set_egress_mac (config, config->wan_sw_if_index,
                                 config->wan_next_hop_mac,
                                 config->wan_next_hop_mac_valid);
  ly_orch_config_set_egress_mac (config, config->lan_sw_if_index,
                                 config->lan_next_hop_mac,
                                 config->lan_next_hop_mac_valid);
  vec_foreach_index (index, config->groups)
    {
      ly_orch_config_set_egress_mac (
        config, config->groups[index].wan_sw_if_index,
        config->groups[index].wan_next_hop_mac,
        config->groups[index].wan_next_hop_mac_valid);
      ly_orch_config_set_egress_mac (
        config, config->groups[index].lan_sw_if_index,
        config->groups[index].lan_next_hop_mac,
        config->groups[index].lan_next_hop_mac_valid);
    }
}

/* Optional trailing next-hop MAC clauses shared by the boundary and group
 * candidate commands:
 *   wan-next-hop-mac <mac> lan-next-hop-mac <mac>
 */
static clib_error_t *
ly_orch_parse_next_hop_macs (unformat_input_t *input, u8 wan_mac[LY_ORCH_MAC_LEN],
                             u8 *wan_valid, u8 lan_mac[LY_ORCH_MAC_LEN],
                             u8 *lan_valid)
{
  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "wan-next-hop-mac %U", unformat_ethernet_address,
                    wan_mac))
        *wan_valid = 1;
      else if (unformat (input, "lan-next-hop-mac %U",
                         unformat_ethernet_address, lan_mac))
        *lan_valid = 1;
      else
        return clib_error_return (0, "expected wan-next-hop-mac <mac> or "
                                     "lan-next-hop-mac <mac>");
    }
  return 0;
}

u32
ly_orch_output_next (u32 sw_if_index)
{
  ly_orch_main_t *om = &ly_orch_main;
  vnet_hw_interface_t *hw =
    vnet_get_sup_hw_interface (om->vnet_main, sw_if_index);
  return vlib_node_add_next (om->vlib_main, ly_orch_node.index,
                             hw->output_node_index);
}

static int
ly_orch_rule_compare (void *left, void *right)
{
  ly_orch_rule_t *a = left;
  ly_orch_rule_t *b = right;
  if (a->group_position != b->group_position)
    return a->group_position < b->group_position ? -1 : 1;
  if (a->sequence != b->sequence)
    return a->sequence < b->sequence ? -1 : 1;
  return strcmp ((char *) a->id, (char *) b->id);
}

static int
ly_orch_find_group (ly_orch_config_t *config, const u8 *name)
{
  u32 index;
  vec_foreach_index (index, config->groups)
    if (strcmp ((char *) config->groups[index].name,
                (const char *) name) == 0)
      return index;
  return -1;
}

static int
ly_orch_interface_is_hardware (u32 sw_if_index)
{
  ly_orch_main_t *om = &ly_orch_main;
  if (pool_is_free_index (om->vnet_main->interface_main.sw_interfaces,
                          sw_if_index))
    return 0;
  return vnet_get_sw_interface (om->vnet_main, sw_if_index)->type ==
         VNET_SW_INTERFACE_TYPE_HARDWARE;
}

static clib_error_t *
ly_orch_candidate_clear (void)
{
  ly_orch_config_free (&ly_orch_main.candidate);
  ly_orch_main.candidate.default_action = LY_ORCH_ACTION_DIRECT;
  return 0;
}

static clib_error_t *
ly_orch_candidate_boundary (unformat_input_t *input)
{
  ly_orch_config_t *candidate = &ly_orch_main.candidate;
  u32 wan = LY_ORCH_INVALID_INDEX, lan = LY_ORCH_INVALID_INDEX;
  clib_error_t *error;
  if (!unformat (input, "wan %U lan %U", unformat_vnet_sw_interface,
                 ly_orch_main.vnet_main, &wan, unformat_vnet_sw_interface,
                 ly_orch_main.vnet_main, &lan))
    return clib_error_return (0, "expected wan <interface> lan <interface>");
  if (wan == lan || !ly_orch_interface_is_hardware (wan) ||
      !ly_orch_interface_is_hardware (lan))
    return clib_error_return (0, "WAN and LAN must be different hardware interfaces");
  if ((error = ly_orch_parse_next_hop_macs (
         input, candidate->wan_next_hop_mac,
         &candidate->wan_next_hop_mac_valid, candidate->lan_next_hop_mac,
         &candidate->lan_next_hop_mac_valid)))
    return error;
  candidate->wan_sw_if_index = wan;
  candidate->lan_sw_if_index = lan;
  candidate->valid = 1;
  return 0;
}

static clib_error_t *
ly_orch_candidate_group (unformat_input_t *input)
{
  ly_orch_config_t *candidate = &ly_orch_main.candidate;
  ly_orch_group_t group = { 0 };
  u8 *name = 0;
  clib_error_t *error;
  u8 wan_mac[LY_ORCH_MAC_LEN] = { 0 }, lan_mac[LY_ORCH_MAC_LEN] = { 0 };
  u8 wan_mac_valid = 0, lan_mac_valid = 0;
  if (!unformat (input, "%s wan-facing %U lan-facing %U", &name,
                 unformat_vnet_sw_interface, ly_orch_main.vnet_main,
                 &group.wan_sw_if_index, unformat_vnet_sw_interface,
                 ly_orch_main.vnet_main, &group.lan_sw_if_index))
    {
      vec_free (name);
      return clib_error_return (
        0, "expected <name> wan-facing <interface> lan-facing <interface>");
    }
  if (!candidate->valid)
    {
      vec_free (name);
      return clib_error_return (0, "candidate boundary is required first");
    }
  if (vec_len (candidate->groups) >= 64 || !name || vec_len (name) >= LY_ORCH_NAME_LEN ||
      ly_orch_find_group (candidate, name) >= 0 ||
      group.wan_sw_if_index == group.lan_sw_if_index ||
      !ly_orch_interface_is_hardware (group.wan_sw_if_index) ||
      !ly_orch_interface_is_hardware (group.lan_sw_if_index))
    {
      vec_free (name);
      return clib_error_return (0, "invalid or duplicate orchestration group");
    }
  if ((error = ly_orch_parse_next_hop_macs (input, wan_mac, &wan_mac_valid,
                                            lan_mac, &lan_mac_valid)))
    {
      vec_free (name);
      return error;
    }
  ly_orch_copy_name (group.name, name);
  vec_free (name);
  if (wan_mac_valid)
    {
      clib_memcpy_fast (group.wan_next_hop_mac, wan_mac, LY_ORCH_MAC_LEN);
      group.wan_next_hop_mac_valid = 1;
    }
  if (lan_mac_valid)
    {
      clib_memcpy_fast (group.lan_next_hop_mac, lan_mac, LY_ORCH_MAC_LEN);
      group.lan_next_hop_mac_valid = 1;
    }
  vec_add1 (candidate->groups, group);
  return 0;
}

static clib_error_t *
ly_orch_candidate_rule (unformat_input_t *input)
{
  ly_orch_config_t *candidate = &ly_orch_main.candidate;
  ly_orch_rule_t rule = { 0 };
  u8 *id = 0, *action = 0, *target = 0, *family = 0;
  ip4_address_t source4 = { 0 }, destination4 = { 0 };
  ip6_address_t source6 = { 0 }, destination6 = { 0 };
  u32 protocol = 0, source_start = 0, source_end = 65535;
  u32 destination_start = 0, destination_end = 65535;
  u32 source_prefix = 0, destination_prefix = 0;
  clib_error_t *error = 0;

  if (!unformat (input,
                 "id %s group-position %u sequence %u action %s target %s "
                 "family %s",
                 &id, &rule.group_position, &rule.sequence, &action, &target,
                 &family))
    {
      error = clib_error_return (0, "invalid rule header");
      goto done;
    }
  if (!candidate->valid || !id || vec_len (id) >= LY_ORCH_NAME_LEN ||
      rule.group_position == 0 || rule.sequence == 0)
    {
      error = clib_error_return (0, "invalid rule identity or ordering");
      goto done;
    }
  if (strcmp ((char *) action, "via") == 0)
    {
      int group = ly_orch_find_group (candidate, target);
      if (group < 0)
        {
          error = clib_error_return (0, "rule target group does not exist");
          goto done;
        }
      rule.action = LY_ORCH_ACTION_VIA;
      rule.target_group = group;
    }
  else if (strcmp ((char *) action, "direct") == 0)
    rule.action = LY_ORCH_ACTION_DIRECT;
  else if (strcmp ((char *) action, "drop") == 0)
    rule.action = LY_ORCH_ACTION_DROP;
  else
    {
      error = clib_error_return (0, "rule action must be via, direct, or drop");
      goto done;
    }

  if (strcmp ((char *) family, "ip4") == 0)
    {
      if (!unformat (input,
                     "src %U/%u dst %U/%u proto %u sport %u-%u dport %u-%u",
                     unformat_ip4_address, &source4, &source_prefix,
                     unformat_ip4_address, &destination4,
                     &destination_prefix, &protocol, &source_start,
                     &source_end, &destination_start, &destination_end) ||
          source_prefix > 32 || destination_prefix > 32)
        {
          error = clib_error_return (0, "invalid IPv4 rule match");
          goto done;
        }
      rule.source.ip4 = source4;
      rule.destination.ip4 = destination4;
    }
  else if (strcmp ((char *) family, "ip6") == 0)
    {
      rule.is_ip6 = 1;
      if (!unformat (input,
                     "src %U/%u dst %U/%u proto %u sport %u-%u dport %u-%u",
                     unformat_ip6_address, &source6, &source_prefix,
                     unformat_ip6_address, &destination6,
                     &destination_prefix, &protocol, &source_start,
                     &source_end, &destination_start, &destination_end) ||
          source_prefix > 128 || destination_prefix > 128)
        {
          error = clib_error_return (0, "invalid IPv6 rule match");
          goto done;
        }
      rule.source.ip6 = source6;
      rule.destination.ip6 = destination6;
    }
  else
    {
      error = clib_error_return (0, "rule family must be ip4 or ip6");
      goto done;
    }
  if (protocol > 255 || source_start > source_end || source_end > 65535 ||
      destination_start > destination_end || destination_end > 65535)
    {
      error = clib_error_return (0, "invalid protocol or port range");
      goto done;
    }
  ly_orch_copy_name (rule.id, id);
  rule.protocol = protocol;
  rule.source_prefix_length = source_prefix;
  rule.destination_prefix_length = destination_prefix;
  rule.source_port_start = source_start;
  rule.source_port_end = source_end;
  rule.destination_port_start = destination_start;
  rule.destination_port_end = destination_end;
  if (vec_len (candidate->rules) >= 4096)
    error = clib_error_return (0, "candidate rule limit exceeded");
  else
    vec_add1 (candidate->rules, rule);

done:
  vec_free (id);
  vec_free (action);
  vec_free (target);
  vec_free (family);
  return error;
}

static int
ly_orch_capture_interface_matches (u32 configured_sw_if_index,
                                   u32 capture_sw_if_index)
{
  bond_if_t *bond =
    bond_get_bond_if_by_sw_if_index (configured_sw_if_index);
  u32 index;

  if (!bond)
    return configured_sw_if_index == capture_sw_if_index;
  vec_foreach_index (index, bond->members)
    if (bond->members[index] == capture_sw_if_index)
      return 1;
  return 0;
}

static int
ly_orch_config_interface_set (ly_orch_config_t *config, u32 sw_if_index)
{
  u32 index;
  if (ly_orch_capture_interface_matches (config->wan_sw_if_index,
                                         sw_if_index) ||
      ly_orch_capture_interface_matches (config->lan_sw_if_index,
                                         sw_if_index))
    return 1;
  vec_foreach_index (index, config->groups)
    if (ly_orch_capture_interface_matches (
          config->groups[index].wan_sw_if_index, sw_if_index) ||
        ly_orch_capture_interface_matches (
          config->groups[index].lan_sw_if_index, sw_if_index))
      return 1;
  return 0;
}

static clib_error_t *
ly_orch_validate_candidate (ly_orch_config_t *candidate)
{
  u32 first, second;
  if (!candidate->valid || candidate->wan_sw_if_index == LY_ORCH_INVALID_INDEX ||
      candidate->lan_sw_if_index == LY_ORCH_INVALID_INDEX)
    return clib_error_return (0, "candidate boundary is incomplete");
  for (first = 0; first < vec_len (candidate->groups); first++)
    {
      ly_orch_group_t *group = &candidate->groups[first];
      if (group->wan_sw_if_index == candidate->wan_sw_if_index ||
          group->wan_sw_if_index == candidate->lan_sw_if_index ||
          group->lan_sw_if_index == candidate->wan_sw_if_index ||
          group->lan_sw_if_index == candidate->lan_sw_if_index)
        return clib_error_return (0, "group interface overlaps WAN or LAN");
      for (second = first + 1; second < vec_len (candidate->groups); second++)
        if (group->wan_sw_if_index == candidate->groups[second].wan_sw_if_index ||
            group->wan_sw_if_index == candidate->groups[second].lan_sw_if_index ||
            group->lan_sw_if_index == candidate->groups[second].wan_sw_if_index ||
            group->lan_sw_if_index == candidate->groups[second].lan_sw_if_index)
          return clib_error_return (0, "group interfaces overlap");
    }
  return 0;
}

static void
ly_orch_workers_free (ly_orch_worker_t *workers)
{
  u32 index;
  vec_foreach_index (index, workers)
    {
      vec_free (workers[index].rule_counters);
      vec_free (workers[index].group_bypass_packets);
      clib_mem_free (workers[index].flows);
    }
  vec_free (workers);
}

static ly_orch_worker_t *
ly_orch_workers_create (u32 rules, u32 groups)
{
  ly_orch_worker_t *workers = 0;
  u32 index, count = vlib_get_n_threads ();
  vec_validate (workers, count - 1);
  for (index = 0; index < count; index++)
    {
      if (rules)
        vec_validate (workers[index].rule_counters, rules - 1);
      if (groups)
        vec_validate (workers[index].group_bypass_packets, groups - 1);
      workers[index].flows = clib_mem_alloc_aligned (
        sizeof (ly_orch_flow_t) * LY_ORCH_FLOW_SLOTS,
        CLIB_CACHE_LINE_BYTES);
      clib_memset (workers[index].flows, 0,
                   sizeof (ly_orch_flow_t) * LY_ORCH_FLOW_SLOTS);
    }
  return workers;
}

static ly_orch_worker_t *
ly_orch_workers_snapshot (u32 rules, u32 groups)
{
  ly_orch_main_t *om = &ly_orch_main;
  ly_orch_worker_t *snapshot = ly_orch_workers_create (rules, groups);
  u32 index;

  vlib_worker_thread_barrier_sync (om->vlib_main);
  vec_foreach_index (index, snapshot)
    {
      if (rules)
        clib_memcpy_fast (snapshot[index].rule_counters,
                          om->workers[index].rule_counters,
                          sizeof (ly_orch_rule_counter_t) * rules);
      if (groups)
        clib_memcpy_fast (snapshot[index].group_bypass_packets,
                          om->workers[index].group_bypass_packets,
                          sizeof (u64) * groups);
      snapshot[index].bypass_packets = om->workers[index].bypass_packets;
      clib_memcpy_fast (snapshot[index].flows, om->workers[index].flows,
                        sizeof (ly_orch_flow_t) * LY_ORCH_FLOW_SLOTS);
    }
  vlib_worker_thread_barrier_release (om->vlib_main);
  return snapshot;
}

static ly_orch_group_bypass_t *ly_orch_group_bypass_build (
  ly_orch_config_t *config);

static clib_error_t *
ly_orch_commit (unformat_input_t *input)
{
  ly_orch_main_t *om = &ly_orch_main;
  ly_orch_config_t *candidate = &om->candidate;
  ly_orch_config_t old_config;
  ly_orch_worker_t *old_workers;
  ly_orch_worker_t *new_workers;
  ly_orch_group_bypass_t *new_group_bypass;
  ly_orch_group_bypass_t *old_group_bypass;
  u8 *generation = 0;
  u32 index, max_sw_if_index = 0;
  clib_error_t *error;

  if (!unformat (input, "generation %s", &generation) || !generation ||
      vec_len (generation) >= LY_ORCH_GENERATION_LEN)
    {
      vec_free (generation);
      return clib_error_return (0, "valid generation is required");
    }
  if ((error = ly_orch_validate_candidate (candidate)))
    {
      vec_free (generation);
      return error;
    }
  vec_sort_with_function (candidate->rules, ly_orch_rule_compare);
  candidate->wan_next_index = ly_orch_output_next (candidate->wan_sw_if_index);
  candidate->lan_next_index = ly_orch_output_next (candidate->lan_sw_if_index);
  vec_foreach_index (index, candidate->groups)
    {
      candidate->groups[index].wan_next_index =
        ly_orch_output_next (candidate->groups[index].wan_sw_if_index);
      candidate->groups[index].lan_next_index =
        ly_orch_output_next (candidate->groups[index].lan_sw_if_index);
    }
  vec_free (candidate->egress_mac);
  candidate->egress_mac = 0;
  ly_orch_config_build_egress_macs (candidate);
  clib_memset (candidate->generation, 0, LY_ORCH_GENERATION_LEN);
  clib_strncpy ((char *) candidate->generation, (const char *) generation,
                LY_ORCH_GENERATION_LEN - 1);
  vec_free (generation);
  new_workers = ly_orch_workers_create (vec_len (candidate->rules),
                                        vec_len (candidate->groups));
  new_group_bypass = ly_orch_group_bypass_build (candidate);

  if (vec_len (om->vnet_main->interface_main.sw_interfaces))
    max_sw_if_index =
      vec_len (om->vnet_main->interface_main.sw_interfaces) - 1;
  vec_validate_init_empty (om->enabled_by_sw_if_index, max_sw_if_index, 0);

  vlib_worker_thread_barrier_sync (om->vlib_main);
  for (index = 0; index < vec_len (om->enabled_by_sw_if_index); index++)
    if (om->enabled_by_sw_if_index[index] &&
        !ly_orch_config_interface_set (candidate, index))
      {
        vnet_feature_enable_disable ("device-input", "ly-route-orchestrator",
                                     index, 0, 0, 0);
        om->enabled_by_sw_if_index[index] = 0;
      }
  for (index = 0; index < vec_len (om->enabled_by_sw_if_index); index++)
    if (ly_orch_config_interface_set (candidate, index) &&
        !om->enabled_by_sw_if_index[index])
      {
        vnet_feature_enable_disable ("device-input", "ly-route-orchestrator",
                                     index, 1, 0, 0);
        om->enabled_by_sw_if_index[index] = 1;
      }
  old_config = om->active;
  old_workers = om->workers;
  old_group_bypass = om->group_bypass;
  om->active = *candidate;
  om->workers = new_workers;
  om->group_bypass = new_group_bypass;
  /* Re-align passive targets with the committed group order; a target whose
   * group disappeared is dropped (its counters no longer have a meaning). */
  for (index = 0; index < vec_len (om->probe_targets); index++)
    {
      int position =
        ly_orch_find_group (&om->active, om->probe_targets[index].group);
      if (position < 0)
        {
          vec_del1 (om->probe_targets, index);
          index--;
          continue;
        }
      om->probe_targets[index].group_position = position;
    }
  clib_memset (candidate, 0, sizeof (*candidate));
  candidate->wan_sw_if_index = LY_ORCH_INVALID_INDEX;
  candidate->lan_sw_if_index = LY_ORCH_INVALID_INDEX;
  candidate->default_action = LY_ORCH_ACTION_DIRECT;
  vlib_worker_thread_barrier_release (om->vlib_main);

  ly_orch_config_free (&old_config);
  ly_orch_workers_free (old_workers);
  vec_free (old_group_bypass);
  return 0;
}

static clib_error_t *
ly_orch_disable (void)
{
  ly_orch_main_t *om = &ly_orch_main;
  ly_orch_config_t old_config;
  ly_orch_worker_t *old_workers;
  ly_orch_group_bypass_t *old_group_bypass;
  u32 index;

  vlib_worker_thread_barrier_sync (om->vlib_main);
  for (index = 0; index < vec_len (om->enabled_by_sw_if_index); index++)
    if (om->enabled_by_sw_if_index[index])
      {
        vnet_feature_enable_disable ("device-input", "ly-route-orchestrator",
                                     index, 0, 0, 0);
        om->enabled_by_sw_if_index[index] = 0;
      }
  old_config = om->active;
  old_workers = om->workers;
  old_group_bypass = om->group_bypass;
  clib_memset (&om->active, 0, sizeof (om->active));
  om->active.wan_sw_if_index = LY_ORCH_INVALID_INDEX;
  om->active.lan_sw_if_index = LY_ORCH_INVALID_INDEX;
  om->workers = 0;
  om->group_bypass = 0;
  vec_free (om->probe_targets);
  om->probe_targets = 0;
  /* Disabling orchestration removes the policy the bypass was shielding, so
   * the flag must not survive into a later, unrelated configuration. */
  om->bypass_active = 0;
  om->bypass_forced = 0;
  clib_memset (om->bypass_cause, 0, sizeof (om->bypass_cause));
  clib_memset (om->bypass_reason, 0, sizeof (om->bypass_reason));
  om->bypass_since = 0;
  vlib_worker_thread_barrier_release (om->vlib_main);

  ly_orch_config_free (&old_config);
  ly_orch_workers_free (old_workers);
  vec_free (old_group_bypass);
  ly_orch_candidate_clear ();
  return 0;
}

static void
ly_orch_bypass_copy (u8 destination[], u32 length, const u8 *source)
{
  clib_memset (destination, 0, length);
  if (source)
    clib_strncpy ((char *) destination, (const char *) source, length - 1);
}

/* Apply a bypass transition.  This deliberately does not touch the candidate
 * or the committed generation: the policy stays configured and is re-armed
 * the moment the bypass is cleared. */
static clib_error_t *
ly_orch_set_bypass (vlib_main_t *vm, u8 active, u8 forced, const u8 *cause,
                    const u8 *reason)
{
  ly_orch_main_t *om = &ly_orch_main;
  vlib_worker_thread_barrier_sync (vm);
  om->bypass_active = active;
  om->bypass_forced = forced;
  ly_orch_bypass_copy (om->bypass_cause, sizeof (om->bypass_cause),
                       cause ? cause : (u8 *) (active ? "auto" : "none"));
  ly_orch_bypass_copy (om->bypass_reason, sizeof (om->bypass_reason),
                       reason ? reason : (u8 *) "none");
  om->bypass_since = active ? vlib_time_now (vm) : 0;
  vlib_worker_thread_barrier_release (vm);
  return 0;
}

static clib_error_t *
ly_orch_bypass_command (unformat_input_t *input, vlib_main_t *vm)
{
  ly_orch_main_t *om = &ly_orch_main;
  u8 *group = 0, *cause = 0, *reason = 0;
  u8 active, forced = 0;

  /* Per-group form: bypass group <name> on|off [cause <token>] [reason <token>] */
  if (unformat (input, "group %s", &group))
    {
      int position;
      ly_orch_group_bypass_t *row;
      if (unformat (input, "on"))
        active = 1;
      else if (unformat (input, "off"))
        active = 0;
      else
        {
          vec_free (group);
          return clib_error_return (0, "expected group <name> bypass on|off");
        }
      while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
        {
          if (unformat (input, "cause %s", &cause))
            ;
          else if (unformat (input, "reason %s", &reason))
            ;
          else
            {
              vec_free (group);
              vec_free (cause);
              vec_free (reason);
              return clib_error_return (
                0, "expected cause <token> or reason <token>");
            }
        }
      if (!om->active.valid)
        {
          vec_free (group);
          vec_free (cause);
          vec_free (reason);
          return clib_error_return (0, "orchestrator is not configured");
        }
      position = ly_orch_find_group (&om->active, group);
      if (position < 0 || position >= vec_len (om->group_bypass))
        {
          clib_error_t *error = clib_error_return (
            0, "unknown orchestration group %s", (char *) group);
          vec_free (group);
          vec_free (cause);
          vec_free (reason);
          return error;
        }
      if (active && !cause)
        cause = format (cause, "%s", "manual");
      if (!reason)
        reason = format (reason, "%s",
                         active ? "manual_force" : "manual_clear");
      forced = active && cause && strcmp ((char *) cause, "manual") == 0;
      vlib_worker_thread_barrier_sync (vm);
      row = &om->group_bypass[position];
      row->active = active;
      row->forced = forced;
      ly_orch_bypass_copy (row->cause, sizeof (row->cause), cause);
      ly_orch_bypass_copy (row->reason, sizeof (row->reason), reason);
      row->since = active ? vlib_time_now (vm) : 0;
      vlib_worker_thread_barrier_release (vm);
      vec_free (group);
      vec_free (cause);
      vec_free (reason);
      return 0;
    }

  if (unformat (input, "on"))
    active = 1;
  else if (unformat (input, "off"))
    active = 0;
  else
    return clib_error_return (0, "expected bypass on|off or bypass group <name> on|off");
  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "cause %s", &cause))
        ;
      else if (unformat (input, "reason %s", &reason))
        ;
      else
        {
          vec_free (cause);
          vec_free (reason);
          return clib_error_return (0, "expected cause <token> or reason <token>");
        }
    }
  if (active && !cause)
    cause = format (cause, "%s", "manual");
  if (!reason)
    reason = format (reason, "%s", active ? "manual_force" : "manual_clear");
  /* Only an operator-forced bypass is terminal.  An automatic bypass stays
   * under the supervisor's recovery threshold. */
  forced = active && cause && strcmp ((char *) cause, "manual") == 0;
  {
    clib_error_t *error =
      ly_orch_set_bypass (vm, active, forced, cause, reason);
    vec_free (cause);
    vec_free (reason);
    return error;
  }
}

/* Install one passive request/response target:
 *   probe group <name> dst <ip> port <n> proto tcp|udp [window <seconds>]
 * The target belongs to the group whose ingress port sees the traffic. */
static int
ly_orch_probe_address_equal (const ip46_address_t *left,
                             const ip46_address_t *right, u8 is_ip6)
{
  if (is_ip6)
    return ip6_address_is_equal (&left->ip6, &right->ip6);
  return ip4_address_is_equal (&left->ip4, &right->ip4);
}

static clib_error_t *
ly_orch_probe_command (unformat_input_t *input)
{
  ly_orch_main_t *om = &ly_orch_main;
  u8 *group = 0, *protocol = 0;
  ip4_address_t address4 = { 0 };
  ip6_address_t address6 = { 0 };
  u8 is_ip6 = 0;
  u32 port = 0, window = 10, index;
  ly_orch_probe_target_t target = { 0 };

  if (unformat (input, "clear"))
    {
      vec_free (om->probe_targets);
      om->probe_targets = 0;
      return 0;
    }
  if (!unformat (input, "group %s", &group))
    return clib_error_return (0, "expected probe group <name> or probe clear");
  if (!unformat (input, "dst %U", unformat_ip4_address, &address4))
    {
      if (unformat (input, "dst %U", unformat_ip6_address, &address6))
        is_ip6 = 1;
      else
        {
          vec_free (group);
          return clib_error_return (0, "expected dst <ipv4|ipv6>");
        }
    }
  if (!unformat (input, "port %u", &port) || !unformat (input, "proto %s", &protocol))
    {
      vec_free (group);
      vec_free (protocol);
      return clib_error_return (0, "expected port <n> proto tcp|udp");
    }
  if (unformat (input, "window %u", &window))
    ;
  if (!om->active.valid)
    {
      vec_free (group);
      vec_free (protocol);
      return clib_error_return (0, "orchestrator is not configured");
    }
  if (port < 1 || port > 65535 || window < 1 || window > 3600 ||
      (strcmp ((char *) protocol, "tcp") != 0 &&
       strcmp ((char *) protocol, "udp") != 0))
    {
      vec_free (group);
      vec_free (protocol);
      return clib_error_return (0, "invalid probe target");
    }
  {
    int position = ly_orch_find_group (&om->active, group);
    if (position < 0)
      {
        clib_error_t *error = clib_error_return (
          0, "unknown orchestration group %s", (char *) group);
        vec_free (group);
        vec_free (protocol);
        return error;
      }
    target.group_position = position;
  }
  ly_orch_copy_name (target.group, group);
  if (is_ip6)
    target.destination.ip6 = address6;
  else
    target.destination.ip4 = address4;
  target.destination_port = (u16) port;
  target.protocol = strcmp ((char *) protocol, "udp") == 0 ? IP_PROTOCOL_UDP
                                                            : IP_PROTOCOL_TCP;
  target.is_ip6 = is_ip6;
  target.window = window;

  /* Replace an identical tuple so its counters restart, otherwise append. */
  vec_foreach_index (index, om->probe_targets)
    {
      ly_orch_probe_target_t *existing = &om->probe_targets[index];
      if (existing->group_position == target.group_position &&
          existing->destination_port == target.destination_port &&
          existing->protocol == target.protocol &&
          existing->is_ip6 == target.is_ip6 &&
          ly_orch_probe_address_equal (&existing->destination,
                                       &target.destination, is_ip6))
        {
          clib_memset (existing, 0, sizeof (*existing));
          *existing = target;
          vec_free (group);
          vec_free (protocol);
          return 0;
        }
    }
  if (vec_len (om->probe_targets) >= LY_ORCH_MAX_PROBE_TARGETS)
    {
      vec_free (group);
      vec_free (protocol);
      return clib_error_return (0, "passive probe target limit exceeded");
    }
  vec_add1 (om->probe_targets, target);
  vec_free (group);
  vec_free (protocol);
  return 0;
}

/* Rebuild the per-group bypass rows in active group order, carrying over the
 * flag of a group with the same name across a commit. */
static ly_orch_group_bypass_t *
ly_orch_group_bypass_build (ly_orch_config_t *config)
{
  ly_orch_main_t *om = &ly_orch_main;
  ly_orch_group_bypass_t *rows = 0;
  u32 index;

  if (!vec_len (config->groups))
    return 0;
  vec_validate (rows, vec_len (config->groups) - 1);
  clib_memset (rows, 0, sizeof (ly_orch_group_bypass_t) * vec_len (config->groups));
  vec_foreach_index (index, config->groups)
    {
      u8 *name = config->groups[index].name;
      u32 old;
      clib_strncpy ((char *) rows[index].name, (const char *) name,
                    LY_ORCH_NAME_LEN - 1);
      vec_foreach_index (old, om->group_bypass)
        if (strcmp ((char *) om->group_bypass[old].name, (char *) name) == 0)
          {
            rows[index] = om->group_bypass[old];
            clib_strncpy ((char *) rows[index].name, (const char *) name,
                          LY_ORCH_NAME_LEN - 1);
            break;
          }
    }
  return rows;
}

static clib_error_t *
ly_orch_set_command_fn (vlib_main_t *vm, unformat_input_t *input,
                        vlib_cli_command_t *cmd)
{
  if (unformat (input, "candidate clear"))
    return ly_orch_candidate_clear ();
  if (unformat (input, "candidate boundary"))
    return ly_orch_candidate_boundary (input);
  if (unformat (input, "candidate group"))
    return ly_orch_candidate_group (input);
  if (unformat (input, "candidate rule"))
    return ly_orch_candidate_rule (input);
  if (unformat (input, "candidate default direct"))
    {
      ly_orch_main.candidate.default_action = LY_ORCH_ACTION_DIRECT;
      return 0;
    }
  if (unformat (input, "candidate default drop"))
    {
      ly_orch_main.candidate.default_action = LY_ORCH_ACTION_DROP;
      return 0;
    }
  if (unformat (input, "bypass"))
    return ly_orch_bypass_command (input, vm);
  if (unformat (input, "probe"))
    {
      clib_error_t *error;
      vlib_worker_thread_barrier_sync (vm);
      error = ly_orch_probe_command (input);
      vlib_worker_thread_barrier_release (vm);
      return error;
    }
  if (unformat (input, "disable"))
    return ly_orch_disable ();
  if (unformat (input, "commit"))
    return ly_orch_commit (input);
  return clib_error_return (0, "unknown orchestrator configuration command");
}

VLIB_CLI_COMMAND (ly_orch_set_command, static) = {
  .path = "set ly-route orchestrator",
  .short_help =
    "set ly-route orchestrator candidate clear|boundary|group|rule|default "
    "[wan-next-hop-mac <mac> lan-next-hop-mac <mac>]; "
    "set ly-route orchestrator commit generation <id>|disable|"
    "bypass on|off [cause <token>] [reason <token>]|"
    "bypass group <name> on|off [cause <token>] [reason <token>]|"
    "probe clear|probe group <name> dst <ip> port <n> proto tcp|udp "
    "[window <seconds>]",
  .function = ly_orch_set_command_fn,
};

static const char *
ly_orch_action_name (ly_orch_action_t action)
{
  if (action == LY_ORCH_ACTION_VIA)
    return "via";
  if (action == LY_ORCH_ACTION_DROP)
    return "drop";
  return "direct";
}

static clib_error_t *
ly_orch_show_command_fn (vlib_main_t *vm, unformat_input_t *input,
                         vlib_cli_command_t *cmd)
{
  ly_orch_main_t *om = &ly_orch_main;
  ly_orch_config_t *config = &om->active;
  ly_orch_worker_t *workers;
  u32 index, thread;
  f64 now = vlib_time_now (vm);
  if (!config->valid)
    {
      vlib_cli_output (vm, "state locked");
      return 0;
    }
  workers = ly_orch_workers_snapshot (vec_len (config->rules),
                                      vec_len (config->groups));
  vlib_cli_output (vm, "state running");
  vlib_cli_output (vm, "generation %s", config->generation);
  {
    u8 *suffix = 0;
    if (config->wan_next_hop_mac_valid)
      suffix = format (suffix, " wan-next-hop-mac %U",
                       format_ethernet_address, config->wan_next_hop_mac);
    if (config->lan_next_hop_mac_valid)
      suffix = format (suffix, " lan-next-hop-mac %U",
                       format_ethernet_address, config->lan_next_hop_mac);
    vec_add1 (suffix, 0);
    vlib_cli_output (vm, "boundary wan %U lan %U%s",
                     format_vnet_sw_if_index_name, om->vnet_main,
                     config->wan_sw_if_index, format_vnet_sw_if_index_name,
                     om->vnet_main, config->lan_sw_if_index, suffix);
    vec_free (suffix);
  }
  vlib_cli_output (vm, "default %s",
                   ly_orch_action_name (config->default_action));
  {
    u64 bypass_packets = 0;
    vec_foreach_index (thread, workers)
      bypass_packets += workers[thread].bypass_packets;
    vlib_cli_output (vm, "bypass state %s",
                     om->bypass_active ? "on" : "off");
    vlib_cli_output (vm, "bypass cause %s",
                     om->bypass_cause[0] ? (char *) om->bypass_cause : "none");
    vlib_cli_output (vm, "bypass reason %s",
                     om->bypass_reason[0] ? (char *) om->bypass_reason
                                          : "none");
    vlib_cli_output (vm, "bypass forced %s",
                     om->bypass_forced ? "yes" : "no");
    vlib_cli_output (vm, "bypass packets %llu", bypass_packets);
  }
  vec_foreach_index (index, config->groups)
    {
      u64 bypass_packets = 0;
      u8 *suffix = 0;
      vec_foreach_index (thread, workers)
        bypass_packets += workers[thread].group_bypass_packets[index];
      if (config->groups[index].wan_next_hop_mac_valid)
        suffix = format (suffix, " wan-next-hop-mac %U",
                         format_ethernet_address,
                         config->groups[index].wan_next_hop_mac);
      if (config->groups[index].lan_next_hop_mac_valid)
        suffix = format (suffix, " lan-next-hop-mac %U",
                         format_ethernet_address,
                         config->groups[index].lan_next_hop_mac);
      vec_add1 (suffix, 0);
      vlib_cli_output (vm, "group %s wan-facing %U lan-facing %U%s",
                       config->groups[index].name,
                       format_vnet_sw_if_index_name, om->vnet_main,
                       config->groups[index].wan_sw_if_index,
                       format_vnet_sw_if_index_name, om->vnet_main,
                       config->groups[index].lan_sw_if_index, suffix);
      vec_free (suffix);
      vlib_cli_output (vm, "group-health %s state %s bypass-packets %llu",
                       config->groups[index].name,
                       vnet_sw_interface_is_up (
                         om->vnet_main,
                         config->groups[index].wan_sw_if_index) &&
                           vnet_sw_interface_is_up (
                             om->vnet_main,
                             config->groups[index].lan_sw_if_index) ?
                         "up" : "bypass",
                       bypass_packets);
      if (index < vec_len (om->group_bypass))
        {
          ly_orch_group_bypass_t *row = &om->group_bypass[index];
          vlib_cli_output (
            vm, "group-bypass %s state %s forced %s cause %s packets %llu "
                "reason %s",
            config->groups[index].name, row->active ? "on" : "off",
            row->forced ? "yes" : "no",
            row->cause[0] ? (char *) row->cause : "none", bypass_packets,
            row->reason[0] ? (char *) row->reason : "none");
        }
    }
  vlib_cli_output (vm, "probe-targets %u", vec_len (om->probe_targets));
  vec_foreach_index (index, om->probe_targets)
    {
      ly_orch_probe_target_t *target = &om->probe_targets[index];
      u64 success = target->synack + target->responses;
      u64 timeouts = target->attempts > success ? target->attempts - success : 0;
      vlib_cli_output (
        vm,
        "group-probe %s dst %U port %u proto %s attempts %llu syn %llu "
        "synack %llu rst %llu responses %llu icmp-unreachable %llu "
        "window %.0f",
        target->group,
        target->is_ip6 ? format_ip6_address : format_ip4_address,
        target->is_ip6 ? (void *) &target->destination.ip6 :
                         (void *) &target->destination.ip4,
        target->destination_port,
        target->protocol == IP_PROTOCOL_UDP ? "udp" : "tcp", target->attempts,
        target->syn, target->synack, target->rst, target->responses,
        target->icmp_unreachable, target->window);
      (void) timeouts;
    }
  vec_foreach_index (index, config->rules)
    {
      u64 packets = 0, bytes = 0;
      vec_foreach_index (thread, workers)
        if (index < vec_len (workers[thread].rule_counters))
          {
            packets += workers[thread].rule_counters[index].packets;
            bytes += workers[thread].rule_counters[index].bytes;
          }
      vlib_cli_output (vm,
                       "policy %s group-position %u sequence %u action %s "
                       "packets %llu bytes %llu",
                       config->rules[index].id,
                       config->rules[index].group_position,
                       config->rules[index].sequence,
                       ly_orch_action_name (config->rules[index].action),
                       packets, bytes);
    }
  vec_foreach_index (thread, workers)
    for (index = 0; index < LY_ORCH_FLOW_SLOTS; index++)
      {
        ly_orch_flow_t *flow = &workers[thread].flows[index];
        u32 group;
        u8 *groups = 0;
        if (!flow->occupied ||
            now - flow->last_seen > LY_ORCH_FLOW_TTL_SECONDS)
          continue;
        for (group = 0; group < flow->group_count; group++)
          groups = format (groups, "%s%s", group ? "," : "",
                           config->groups[flow->groups[group]].name);
        vec_add1 (groups, 0);
        vlib_cli_output (
          vm,
          "flow family %s src %U dst %U proto %u sport %u dport %u "
          "packets %llu bytes %llu age %.6f groups %s",
          flow->is_ip6 ? "ip6" : "ip4",
          flow->is_ip6 ? format_ip6_address : format_ip4_address,
          flow->is_ip6 ? (void *) &flow->source.ip6 :
                         (void *) &flow->source.ip4,
          flow->is_ip6 ? format_ip6_address : format_ip4_address,
          flow->is_ip6 ? (void *) &flow->destination.ip6 :
                         (void *) &flow->destination.ip4,
          flow->protocol, flow->source_port, flow->destination_port,
          flow->packets, flow->bytes, now - flow->last_seen,
          groups ? groups : (u8 *) "-");
        vec_free (groups);
      }
  ly_orch_workers_free (workers);
  return 0;
}

VLIB_CLI_COMMAND (ly_orch_show_command, static) = {
  .path = "show ly-route orchestrator",
  .short_help = "show ly-route orchestrator",
  .function = ly_orch_show_command_fn,
};

static clib_error_t *
ly_orch_init (vlib_main_t *vm)
{
  ly_orch_main_t *om = &ly_orch_main;
  om->vlib_main = vm;
  om->vnet_main = vnet_get_main ();
  ly_orch_config_free (&om->active);
  ly_orch_config_free (&om->candidate);
  om->candidate.default_action = LY_ORCH_ACTION_DIRECT;
  ly_orch_mac_table_init ();
  return 0;
}

VLIB_INIT_FUNCTION (ly_orch_init);

VNET_FEATURE_INIT (ly_orch_input_feature, static) = {
  .arc_name = "device-input",
  .node_name = "ly-route-orchestrator",
  .runs_before = VNET_FEATURES ("ethernet-input"),
};
