import unittest

from quickauth import normalize_user_info, valid_relay_discovery


class RelayDiscoveryTests(unittest.TestCase):
    def test_quick_tenant_hosts(self):
        for cell in ("cell001", "qbs-cell01234567-89ab-cdef-0123-456789abcdef"):
            with self.subTest(cell=cell):
                self.assertTrue(valid_relay_discovery({
                    "idcRegion": ["us-east-1"],
                    "tenantUrl": [f"https://{cell}.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev"],
                }))

    def test_untrusted_tenant_hosts(self):
        tenant = "qbs-cell01234567-89ab-cdef-0123-456789abcdef.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev"
        for url in (f"http://{tenant}", f"https://{tenant}.example.test", f"https://{tenant}:8443", "https://127.0.0.1"):
            with self.subTest(url=url):
                self.assertFalse(valid_relay_discovery({"tenantUrl": [url]}))

    def test_normalize_user_info_preserves_usage_summary(self):
        summary = {
            "entitlementStatus": "ALLOWED",
            "monthlyUsage": {"availableUnits": 597, "provisionedUnits": 600, "resetsAt": 1796083200, "usedPercentage": 0},
            "sessionUsage": {"resumeInMinutes": 0, "usedPercentage": 12.5},
        }
        info = normalize_user_info({"email": "user@example.test", "usageSummary": summary})
        self.assertEqual(info["email"], "user@example.test")
        self.assertEqual(info["usage_summary"], summary)