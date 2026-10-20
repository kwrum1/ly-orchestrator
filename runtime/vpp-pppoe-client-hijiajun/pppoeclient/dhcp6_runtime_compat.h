/* SPDX-License-Identifier: Apache-2.0 */
/* Minimal 26.06 compatibility types for the 26.10 pppoeclient API. */
#pragma once
#include <vnet/ip/ip6.h>

typedef struct
{
  u8 enabled;
  u8 rebinding;
  u32 server_index;
  u32 address_count;
  u32 T1;
  u32 T2;
  u32 t1_remaining;
  u32 t2_remaining;
  u32 dns_server_count;
  ip6_address_t dns_servers[2];
  u8 first_address_present;
  ip6_address_t first_address;
  u32 first_address_preferred_lt;
  u32 first_address_valid_lt;
} dhcp6_ia_na_client_runtime_t;

typedef struct
{
  u8 enabled;
  u8 rebinding;
  u32 server_index;
  u32 prefix_count;
  u32 T1;
  u32 T2;
  u32 t1_remaining;
  u32 t2_remaining;
  char *prefix_group;
} dhcp6_pd_client_runtime_t;

typedef struct
{
  u8 present;
  u8 prefix_length;
  ip6_address_t prefix;
  u32 preferred_lt;
  u32 valid_lt;
  u32 valid_remaining;
} dhcp6_pd_active_prefix_runtime_t;

typedef struct
{
  u8 present;
  u32 sw_if_index;
  ip6_address_t address;
  u8 prefix_length;
  u32 consumer_count;
} dhcp6_pd_consumer_runtime_t;
