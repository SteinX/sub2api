"""Offline checks for the probe's redaction and declared-tool constraints."""

import json
import unittest
from probe_server import digest, probe_tool, summarize


class ProbeContractTests(unittest.TestCase):
    def test_summary_drops_prompt_credentials_tool_payload_and_session_text(self):
        body = {
            "model": "fusion-probe", "stream": True,
            "messages": [{"role": "system", "content": "synthetic-private-system"},
                         {"role": "user", "content": "synthetic-private-prompt"}],
            "tools": [{"type": "function", "function": {
                "name": "read", "description": "synthetic-private-tool-description",
                "parameters": {"type": "object", "properties": {"path": {"type": "string"}}},
            }}],
        }
        headers = {"authorization": "Bearer synthetic-private-key", "x-litellm-session-id": "synthetic-private-session"}
        summary = summarize(body, headers, 1)
        rendered = str(summary)
        for private in ("synthetic-private-system", "synthetic-private-prompt", "synthetic-private-tool-description", "synthetic-private-key", "synthetic-private-session"):
            self.assertNotIn(private, rendered)
        self.assertEqual(summary["session_headers"]["x-litellm-session-id"], digest("synthetic-private-session"))
        self.assertEqual(summary["message_roles"], ["system", "user"])
        self.assertEqual(summary["tool_names"], ["read"])

    def test_instruction_and_tool_choice_digests_track_contract_changes(self):
        body = {"messages": [{"role": "system", "content": "system-a"},
                             {"role": "user", "content": "user-a"}], "tool_choice": "auto"}
        first = summarize(body, {}, 1)
        body["messages"][1]["content"] = "user-b"
        self.assertEqual(first["system_instructions_digest"], summarize(body, {}, 2)["system_instructions_digest"])
        body["messages"][0]["content"] = "system-b"
        self.assertNotEqual(first["system_instructions_digest"], summarize(body, {}, 3)["system_instructions_digest"])
        body["tool_choice"] = "none"
        self.assertNotEqual(first["tool_choice_digest"], summarize(body, {}, 4)["tool_choice_digest"])

    def test_stub_only_requests_declared_read_and_stops_after_result(self):
        tool = {"type": "function", "function": {"name": "read", "parameters": {
            "type": "object", "required": ["path"], "properties": {"path": {"type": "string"}},
        }}}
        body = {"tools": [tool], "messages": [{"role": "user", "content": "probe"}]}
        self.assertEqual(probe_tool(body)["function"]["name"], "read")
        body["messages"].append({"role": "tool", "content": "fixture"})
        self.assertIsNone(probe_tool(body))
        self.assertIsNone(probe_tool({"tools": [{"function": {"name": "bash"}}]}))
        tool["function"]["parameters"]["required"].append("unknown_parameter")
        self.assertIsNone(probe_tool({"tools": [tool]}))

    def test_omp_required_intent_is_supplied_only_for_string_schema(self):
        parameters = {"type": "object", "required": ["path", "i"], "properties": {
            "path": {"type": "string"}, "i": {"type": "string"},
        }}
        body = {"tools": [{"function": {"name": "read", "parameters": parameters}}]}
        arguments = json.loads(probe_tool(body)["function"]["arguments"])
        self.assertEqual(arguments["i"], "read synthetic probe fixture")
        self.assertTrue(arguments["path"].endswith("/probe.txt"))
        parameters["properties"]["i"]["type"] = "number"
        self.assertIsNone(probe_tool(body))


if __name__ == "__main__":
    unittest.main()
