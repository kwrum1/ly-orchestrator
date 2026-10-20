/* SPDX-License-Identifier: Apache-2.0 */
/* Minimal VPP 26.06 compatibility shim for the pppoeclient plugin. */
#pragma once
#include <vlib/threads.h>

typedef struct
{
  u32 node_index;
  u32 queue_size;
  u32 dequeue_vector_limit;
} vlib_handoff_alloc_queues_args_t;

static_always_inline u32
vlib_handoff_alloc_queues (vlib_handoff_alloc_queues_args_t *a)
{
  return vlib_frame_queue_main_init (a->node_index, a->queue_size);
}
