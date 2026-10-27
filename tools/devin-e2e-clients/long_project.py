"""One bounded synthetic project; no services, credentials or tool execution.

128 KiB of files is a pressure target, not proof of compaction. Evidence keeps
fingerprints/counts, never the random facts or full client/model output.
"""
import hashlib
import json
import os
from pathlib import Path
import re

PARTS = 8
PART_BYTES = 16 * 1024
LINE_PREFIX = re.compile(r'(?m)^[ \t]*[0-9]+(?:\||:) ?')
FACT_PAIR = re.compile(r'\b(part0[0-7])\b[\s\"\'`]*[:=][\s\"\'`]*([a-f0-9]{32})\b')
SOURCE = '''import json
from pathlib import Path

def discounted(subtotal, discount):
    return subtotal + discount

def report():
    data = json.loads(Path("requirements.json").read_text())
    rows = []
    for name in data["parts"]:
        lines = Path(name).read_text().splitlines()
        rows.extend(json.loads(lines[1].split("=", 1)[1]))
    result = {"nonce": data["nonce"], "totals": [discounted(a,b) for a,b in rows]}
    Path("out").mkdir(exist_ok=True)
    Path("out/report.json").write_text(json.dumps(result))
    return result
'''


def sha(data):
    return hashlib.sha256(data).hexdigest()


def generate(workspace, data):
    context = workspace / 'context'
    context.mkdir()
    rows, facts, names = [], {}, []
    for index in range(PARTS):
        label = f'part{index:02d}'
        fact = os.urandom(16).hex()
        values = [[100 + int.from_bytes(os.urandom(2), 'big'), 1 + byte] for byte in os.urandom(8)]
        head = f'{label}={fact}\nROWS={json.dumps(values, separators=(",", ":"))}\n'.encode()
        tail = f'END_{label}={os.urandom(16).hex()}\n'.encode()
        length = PART_BYTES - len(head) - len(tail)
        noise = os.urandom(length).hex()[:length]
        # Many short lines keep the whole file within the CLI's ordinary read
        # limits, rather than relying on a single truncated 16 KiB line.
        padding = ''.join('\n' if position % 64 == 63 else char for position, char in enumerate(noise)).encode()
        padding = padding[:-1] + b'\n'
        raw = head + padding + tail
        assert len(raw) == PART_BYTES
        path = f'context/{label}.txt'
        with (workspace / path).open('xb') as out:
            out.write(raw)
        names.append(path); rows.extend(values); facts[label] = fact
    data.update(parts=names, rows=rows)
    return {'nonce': data['nonce'], 'totals': [a-b for a,b in rows]}


def fact_values(read):
    return {f'part{index:02d}': read(f'context/part{index:02d}.txt').decode().splitlines()[0].split('=',1)[1] for index in range(PARTS)}


def normalize(text):
    return LINE_PREFIX.sub('', text.replace('\r\n', '\n'))


class ReadEvidence:
    def __init__(self, workspace, read):
        self.workspace = Path(workspace)
        self.parts = {f'context/part{index:02d}.txt': read(f'context/part{index:02d}.txt') for index in range(PARTS)}
        self.calls = {}

    def observe(self, event, tool):
        if not tool or tool['name'] != 'read' or tool['status'] != 'completed':
            return
        part = event.get('part', {})
        state = part.get('state', {})
        inputs, text = state.get('input', {}), state.get('output')
        if not isinstance(inputs, dict) or not isinstance(text, str):
            return
        path = inputs.get('filePath')
        if not isinstance(path, str):
            return
        absolute = os.path.abspath(self.workspace / path)
        for relative, raw in self.parts.items():
            if absolute == str(self.workspace / relative):
                complete = raw.decode() in normalize(text)
                self.calls[tool['call_sha256']] = {'part': relative, 'file_sha256': sha(raw), 'bytes': len(raw),
                    'output_sha256': sha(text.encode()), 'complete': complete, 'call_sha256': tool['call_sha256']}

    def report(self):
        complete = {call['part'] for call in self.calls.values() if call['complete']}
        return {'scope': 'actual completed OpenCode read outputs; not native model context or compaction',
                'complete': len(complete) == PARTS, 'parts': list(self.calls.values()),
                'unique_complete_parts': len(complete), 'complete_payload_bytes': sum(len(self.parts[name]) for name in complete),
                'file_sha256': {name: sha(raw) for name, raw in self.parts.items()},
                'compression': 'not_claimed'}


def recall(text, facts):
    observed = {}
    for label, value in FACT_PAIR.findall(text):
        observed.setdefault(label, set()).add(value)
    passed = (set(observed) == set(facts) and all(values == {facts[label]} for label, values in observed.items()))
    return {'scope': 'latest normally terminated assistant text; expected values withheld from resume prompt',
            'passed': passed, 'expected_facts': len(facts), 'observed_labels': len(observed),
            'expected_sha256': sha(json.dumps(facts, sort_keys=True).encode()), 'final_text_sha256': sha(text.encode()),
            'contradictory_values': any(len(values) != 1 for values in observed.values())}


def prompt(resume):
    if resume:
        return ('Resume this bounded long project session. Without calling ANY tools or reading any files, recall the eight '
                'previously observed random facts part00 through part07 from the retained conversation. Return each label '
                'and its exact value (JSON or label=value is fine). Do not guess. This is a BOUNDED_LONG_RECALL checkpoint.')
    return ('BOUNDED_LONG_PROJECT: Read price_math.py and all eight context/part00.txt through context/part07.txt files '
            'completely, retaining each part00..part07 random fact. Repair only the calculation in price_math.py, run '
            'python3 run_tests.py and read out/report.json to verify all cross-file totals. Keep the facts only in conversation history, never add them to the artifact or another file. '
            'Do not change requirements, tests, runner or context files. Use at most12 public tools: source read1, data reads8, '
            'one edit, one test command and one artifact read. Lead: delegate implementation to Sidekick, review its report '
            'and finish with a concise verified summary. A later same-session restart will ask you to recall the facts '
            'without tools; the expected values will not be in that prompt.')
