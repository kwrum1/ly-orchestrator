#!/usr/bin/env python3
"""Unit tests for the PPPoE plugin's IPv6CP readback and DHCPv6-PD status contract.

Run: python3 scripts/test-pppoe-plugin-ipv6.py
"""
import importlib.machinery
import importlib.util
import json
import os
import sys
import tempfile

PLUGIN = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "packaging/rootfs-overlay/usr/lib/ly-route/ly-route-pppoe-plugin",
)

loader = importlib.machinery.SourceFileLoader("lyroute_pppoe_plugin", PLUGIN)
spec = importlib.util.spec_from_loader(loader.name, loader)
plugin = importlib.util.module_from_spec(spec)
loader.exec_module(plugin)

DETAIL = """
PPPOE_CLIENT_SESSION 1
    session-id 7
    runtime ipv4 local 100.64.0.2 peer 100.64.0.1
    runtime pppox-interface lyppp-abcd1234 sw-if-index 12 unit 1 session-allocated 1
    runtime ipv6cp-link-local local fe80::1 peer fe80::2
    runtime wan-ipv6 observed 2001:db8:1::1/64
    runtime wan-ipv6-mode global-address-observed peer-host-route 1 default-route4 1 default-route6 1
    ac-mac 00:11:22:33:44:55
"""

DETAIL_NO_V6 = """
PPPOE_CLIENT_SESSION 1
    session-id 8
    runtime ipv4 local 100.64.0.4 peer 100.64.0.3
    runtime ipv6cp-link-local local :: peer ::
    runtime wan-ipv6 observed <none>
"""

PREFIXES = """opaque_data: 12, prefix: 2001:db8:aa00::/56, prefix group: lyroute-pd-wan1, preferred lifetime: 3600, valid lifetime: 7200 (7100.5 remaining)
"""

failures = []


def check(name, condition, detail=""):
    if condition:
        print(f"ok   {name}")
    else:
        print(f"FAIL {name} {detail}")
        failures.append(name)


session = plugin.parse_detail(DETAIL)
check("parse_detail session id", session and session["session_id"] == 7)
check("parse_detail ipv4", session and session["local"] == "100.64.0.2")
check("parse_detail interface", session and session["interface"] == "lyppp-abcd1234")
check("ipv6 ready", session and session["ipv6"]["ready"] is True)
check("ipv6 local", session and session["ipv6"]["local"] == "fe80::1")
check("ipv6 global", session and session["ipv6"]["global"] == "2001:db8:1::1/64")

no_v6 = plugin.parse_detail(DETAIL_NO_V6)
check("ipv6 not ready without IPv6CP", no_v6 and no_v6["ipv6"]["ready"] is False)
check("no global prefix", no_v6 and not no_v6["ipv6"].get("global"))

check("pd group name", plugin.pd_group_name("wan1") == "lyroute-pd-wan1")
check("pd group name sanitised", plugin.pd_group_name("wan/1 x") == "lyroute-pd-wan-1-x")

original = plugin.vppctl
plugin.vppctl = lambda *a, **k: (0, PREFIXES, "")
pd = plugin.pd_prefix("lyroute-pd-wan1")
check("pd prefix parsed", pd == {"prefix": "2001:db8:aa00::/56", "preferred": 3600, "valid": 7200}, str(pd))
plugin.vppctl = original

with tempfile.TemporaryDirectory() as tmp:
    status_path = os.path.join(tmp, "wan1.json")
    plugin.write_status(
        status_path,
        "connected",
        "lyppp-abcd1234",
        session_id=7,
        local="100.64.0.2",
        peer="100.64.0.1",
        ac_mac="00:11:22:33:44:55",
        ipv6=session["ipv6"],
        pd={"prefix": "2001:db8:aa00::/56", "preferred": 3600, "valid": 7200},
    )
    payload = json.load(open(status_path, encoding="utf-8"))
    sess = payload.get("session", {})
    check("status ipv6_ready", sess.get("ipv6_ready") is True)
    check("status local_ipv6", sess.get("local_ipv6") == "fe80::1")
    check("status remote_ipv6", sess.get("remote_ipv6") == "fe80::2")
    check("status delegated prefix", sess.get("delegated_prefix") == "2001:db8:aa00::/56")
    check("status preferred lifetime", sess.get("prefix_preferred_lifetime") == 3600)
    check("status valid lifetime", sess.get("prefix_valid_lifetime") == 7200)

    plugin.write_status(status_path, "connected", "lyppp-abcd1234", 7, "100.64.0.2", "100.64.0.1",
                        "00:11:22:33:44:55", ipv6={"ready": False})
    sess = json.load(open(status_path, encoding="utf-8"))["session"]
    check("status clears prefix on disconnect", "delegated_prefix" not in sess)
    check("status ipv6_ready false", sess.get("ipv6_ready") is False)

print()
if failures:
    print(f"FAILED {len(failures)}: {', '.join(failures)}")
    sys.exit(1)
print("all pppoe ipv6/pd contract checks passed")
