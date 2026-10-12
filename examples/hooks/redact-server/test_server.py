import unittest
from server import recursively_redact, redact_string


class TestRedactionServer(unittest.TestCase):
    def test_redact_string_patterns(self) -> None:
        # OpenAI style
        s1 = "Here is my key: sk-abcdef1234567890abcdef1234567890 for API calls"
        r1, mod1 = redact_string(s1)
        self.assertTrue(mod1)
        self.assertNotIn("sk-", r1)
        self.assertIn("[REDACTED]", r1)

        # Fake test key
        s2 = "Found sk_test_not_a_real_key_123 in configuration"
        r2, mod2 = redact_string(s2)
        self.assertTrue(mod2)
        self.assertIn("[REDACTED]", r2)

        # AWS Key
        s3 = "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"
        r3, mod3 = redact_string(s3)
        self.assertTrue(mod3)
        self.assertIn("[REDACTED]", r3)

        # Clean string
        s4 = "Hello world, no secrets here"
        r4, mod4 = redact_string(s4)
        self.assertFalse(mod4)
        self.assertEqual(r4, s4)

    def test_recursively_redact_nested_structures(self) -> None:
        data = {
            "content": [
                {"type": "text", "text": "secret: ghp_123456789012345678901234567890123456"},
                {"type": "info", "text": "normal text"},
            ],
            "metadata": {"user": "alice"},
        }

        redacted_data, modified = recursively_redact(data)
        self.assertTrue(modified)
        self.assertEqual(redacted_data["content"][0]["text"], "secret: [REDACTED]")
        self.assertEqual(redacted_data["content"][1]["text"], "normal text")
        self.assertEqual(redacted_data["metadata"]["user"], "alice")


if __name__ == "__main__":
    unittest.main()
