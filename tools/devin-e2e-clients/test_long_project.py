import json
from pathlib import Path
import tempfile
import unittest

import long_project as long
from project_completion import CompletionEvidence


class BoundedLongTests(unittest.TestCase):
    def create(self, root):
        data={'nonce':'synthetic-nonce'}
        expected=long.generate(root,data)
        read=lambda name:(root/name).read_bytes()
        return data,expected,read

    def test_exact_eight_file_dataset_with_no_fact_values_in_requirements_or_artifact(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);data,expected,read=self.create(root)
            self.assertEqual(8,len(data['parts']))
            self.assertEqual(128*1024,sum(len(read(name)) for name in data['parts']))
            facts=long.fact_values(read)
            self.assertEqual(8,len(set(facts.values())))
            for value in facts.values():
                self.assertNotIn(value,json.dumps(data));self.assertNotIn(value,json.dumps(expected));self.assertNotIn(value,long.SOURCE)
            self.assertEqual(64,len(expected['totals']))
            self.assertNotIn('facts',expected)

    def test_actual_completed_reads_require_full_protected_content(self):
        for scenario in ('complete','missing','truncated','modified','not_completed'):
            with self.subTest(scenario=scenario),tempfile.TemporaryDirectory() as temporary:
                root=Path(temporary);data,_,read=self.create(root);proof=long.ReadEvidence(root,read)
                for index,path in enumerate(data['parts']):
                    if scenario=='missing' and index==7:continue
                    raw=read(path).decode()
                    if scenario=='truncated' and index==7:raw=raw[:-50]
                    if scenario=='modified' and index==7:raw=raw[:300]+('0' if raw[300]!='0' else '1')+raw[301:]
                    output='<content>\n'+'\n'.join(f'{i:05d}| {line}' for i,line in enumerate(raw.splitlines(),1))+'\n</content>'
                    tool={'name':'read','status':'running' if scenario=='not_completed' else 'completed','call_sha256':str(index)*64}
                    proof.observe({'type':'tool_use','part':{'state':{'input':{'filePath':str(root/path)},'output':output}}},tool)
                report=proof.report()
                self.assertEqual(scenario=='complete',report['complete'])
                self.assertEqual('not_claimed',report['compression'])

    def test_recall_accepts_plain_or_fenced_json_but_rejects_conflicts_and_missing_facts(self):
        facts={f'part{i:02d}':str(i)*32 for i in range(8)}
        for text in [json.dumps(facts),'```json\n'+json.dumps(facts)+'\n```','\n'.join(f'{k}={v}' for k,v in facts.items())]:
            self.assertTrue(long.recall(text,facts)['passed'])
        for text in [json.dumps({**facts,'part00':'f'*32}),json.dumps({k:v for k,v in facts.items() if k!='part07'}),json.dumps(facts)+'\npart00='+('f'*32)]:
            self.assertFalse(long.recall(text,facts)['passed'])
        self.assertNotIn(facts['part01'],json.dumps(long.recall(json.dumps(facts),facts)))

    def test_early_correct_facts_cannot_supply_a_later_error_terminal(self):
        facts={f'part{i:02d}':str(i)*32 for i in range(8)}
        completion=CompletionEvidence()
        completion.observe({'type':'step_start'})
        completion.observe({'type':'text','part':{'id':'first','text':json.dumps(facts)}})
        completion.observe({'type':'step_finish','part':{'reason':'tool-calls'}})
        completion.observe({'type':'step_start'})
        completion.observe({'type':'text','part':{'id':'last','text':'Unable to finish.'}})
        completion.observe({'type':'step_finish','part':{'reason':'error'}})
        self.assertFalse(completion.normal_stop)
        self.assertFalse(long.recall(''.join(completion.parts.values()),facts)['passed'])

    def test_resume_prompt_contains_labels_but_never_values_or_a_read_instruction(self):
        prompt=long.prompt(True)
        self.assertIn('Without calling ANY tools',prompt)
        self.assertIn('part00 through part07',prompt)
        self.assertNotIn('out/report.json',prompt)


if __name__=='__main__':unittest.main()
