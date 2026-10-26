"""Optional actual OMP RPC capabilities over the already copied product config.

Only synthetic project data is used. Reports retain structural facts, not RPC
messages or summaries. A failed capability remains failed, never a task pass.
"""
import json
import os
from pathlib import Path
import queue
import re
import signal
import subprocess
import threading
import time
from urllib.parse import urlsplit
import batch_control
from project_evidence import ToolEvidence


class Rpc:
    def __init__(self, args, directory, helpers):
        self.h = helpers
        self.events = queue.Queue(maxsize=20000)
        self.frames = self.parse_errors = self.reads = 0
        self.final_text = ''
        self.failed = False
        self.error_categories = set()
        self.error_http_status = None
        self.error_vocabulary = set()
        self.serial = 0
        self.control = batch_control.current()
        self.tools = ToolEvidence(directory/'workspace', helpers.TEST_COMMAND)
        self.deadline = time.monotonic() + 180
        env = helpers.project_environment(directory)
        env['SUB2API_API_KEY'] = os.environ['DEVIN_JOURNEY_GATEWAY_KEY']
        env['BUN_INSTALL_CACHE_DIR'] = str(directory / 'cache/bun')
        command = helpers.command('omp', args.model, True)[:-1]
        self.trace = None
        if os.environ.get('DEVIN_JOURNEY_FETCH_DIAGNOSTIC') == 'true':
            if os.environ.get('DEVIN_E2E_MODE') != 'mock':
                raise ValueError('fetch_diagnostic_requires_mock')
            origin = urlsplit(args.ui_url)
            env['DEVIN_JOURNEY_FETCH_ORIGIN'] = f'{origin.scheme}://{origin.netloc}'
            self.trace = directory / f'rpc-fetch-{time.time_ns()}.jsonl'
            with os.fdopen(os.open(self.trace,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600),'w'): pass
            if os.geteuid() == 0:
                os.chown(self.trace, 65534, 65534)
            env['DEVIN_JOURNEY_FETCH_TRACE'] = str(self.trace)
            command[1:1] = ['--preload', str(helpers.ROOT / 'diagnostic-fetch.cjs')]
        command.remove('-p')
        command[command.index('--mode') + 1] = 'rpc'
        self.proc=helpers.start_child(command,directory,env,gateway=args.ui_url,client='omp',stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
        self.reader = threading.Thread(target=self.consume, daemon=True)
        self.reader.start()

    def consume(self):
        size = 0
        for raw in iter(lambda: self.proc.stdout.readline(4 * 1024 * 1024 + 1), b''):
            size += len(raw)
            if len(raw) > 4 * 1024 * 1024 or size > 32 * 1024 * 1024:
                self.failed = True
                self.control.stop('reader_error')
                self.h.kill_child(self.proc)
                return
            try:
                event = json.loads(raw)
                tool = self.tools.observe(event)
                self.control.tool(tool)
                self.control.observe_terminal(event)
                if self.control.stopped:
                    self.failed=True
                    self.h.kill_child(self.proc)
                    return
                self.events.put(event, timeout=1)
            except (ValueError, UnicodeDecodeError, queue.Full):
                self.parse_errors += 1
                self.control.stop('parse_error')
                self.h.kill_child(self.proc)
                return

    def call(self, kind, local_prompt=False, **fields):
        model_operation = kind in {'prompt', 'compact'}
        if model_operation:
            self.control.begin()
        try:
            result = self._call(kind, local_prompt, **fields)
            if model_operation and not local_prompt:
                self.control.end(True)
            return result
        except (ValueError, OSError):
            self.control.stop('protocol_error')
            self.h.kill_child(self.proc)
            raise

    def _call(self, kind, local_prompt=False, **fields):
        self.serial += 1
        ident = str(self.serial)
        self.proc.stdin.write((json.dumps({'id': ident, 'type': kind, **fields}) + '\n').encode())
        self.proc.stdin.flush()
        end = min(self.deadline, time.monotonic() + 90)
        while time.monotonic() < end:
            try:
                event = self.events.get(timeout=min(1, max(.01, end-time.monotonic())))
            except queue.Empty:
                if self.proc.poll() is not None:
                    raise ValueError('rpc_process_exited')
                continue
            self.frames += 1
            error_text = str(event.get('error', {}).get('message', '')) if isinstance(event.get('error'), dict) else str(event.get('error', ''))
            error_text += ' ' + str(event.get('message', {}).get('errorMessage', '')) if isinstance(event.get('message'), dict) else ''
            for category, pattern in {'context_limit': r'context.{0,80}(limit|window|full|large|exceed)', 'token_limit': r'(token.{0,50}(limit|exceed)|maximum.{0,50}token)',
                                      'tool_call_validation': r'tool.{0,60}(invalid|duplicate|not found|validation)', 'budget': r'budget|quota|balance',
                                      'http_error': r'http|status code', 'model_error': r'model.{0,40}(error|unknown|unavailable)',
                                      'transport_error': r'connect|socket|stream|network', 'parse_error': r'parse|json|unexpected',
                                      'disabled_capability': r'disabled|not enabled|unsupported', 'nothing_to_compact': r'nothing to compact|no messages|not enough|already compact',
                                      'missing_role_or_credentials': r'api key|credentials|role|not configured'} .items():
                if re.search(pattern, error_text, re.IGNORECASE):
                    self.error_categories.add(category)
            vocabulary = {'compaction', 'summary', 'empty', 'no', 'model', 'selected', 'failed', 'error', 'too', 'small', 'nothing', 'to', 'compact',
                          'disabled', 'aborted', 'cancelled', 'output', 'text', 'response', 'received', 'missing', 'expected', 'not', 'found',
                          'cannot', 'could', 'format', 'valid', 'json', 'rpc', 'schema', 'invalid', 'read', 'undefined', 'null', 'content', 'data',
                          'attempt', 'max', 'tokens', 'key', 'provider', 'api', 'limit', 'exceeded', 'token', 'input', 'required', 'state',
                          'manual', 'method', 'configured', 'run', 'history', 'validation', 'available', 'backend', 'method', 'source', 'assertion'}
            self.error_vocabulary.update(word for word in re.findall(r'[a-z]+', error_text.lower()) if word in vocabulary)
            if isinstance(event.get('error'), dict):
                self.error_http_status = event['error'].get('httpStatus', self.error_http_status)
            if event.get('type') == 'tool_execution_end':
                self.failed |= bool(event.get('isError'))
                self.reads += event.get('toolName') in {'read', 'read_file'} and event.get('isError') is False
            if event.get('type') == 'message_end' and event.get('message', {}).get('role') == 'assistant':
                message = event['message']
                self.final_text = ''.join(part.get('text', '') for part in message.get('content', []) if part.get('type') == 'text')
                self.failed |= message.get('stopReason') in {'error', 'aborted'}
            if event.get('id') != ident:
                continue
            if event.get('type') == 'response' and event.get('success') is not True:
                raise ValueError('rpc_command_rejected')
            if kind == 'prompt':
                # OMP 18.12.6 discards the prompt lifecycle token for locally
                # handled slash commands and returns this response instead of
                # a prompt_result. Acknowledge only this exact local command;
                # run() independently verifies a newly persisted checkpoint.
                if (local_prompt and fields.get('message') == '/compact soft' and event.get('type') == 'response'
                        and event.get('success') is True and event.get('data', {}).get('agentInvoked') is False):
                    if self.failed or self.parse_errors:
                        raise ValueError('rpc_event_failure')
                    return {'local_command_acknowledged': True}
                if event.get('type') != 'prompt_result':
                    continue
                invoked = event.get('agentInvoked') is True or (local_prompt and fields.get('message') == '/compact soft' and event.get('agentInvoked') is False)
                if event.get('status') != 'completed' or not invoked or event.get('sessionSettled') is not True:
                    raise ValueError('rpc_prompt_not_completed')
            elif event.get('type') != 'response':
                continue
            if self.failed or self.parse_errors:
                raise ValueError('rpc_event_failure')
            return event.get('data', {})
        raise ValueError('rpc_timeout')

    def close(self, abrupt=False):
        clean = True
        try:
            if self.proc.poll() is None:
                os.killpg(self.proc.pid, signal.SIGKILL if abrupt else signal.SIGTERM)
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(self.proc.pid, signal.SIGKILL)
                self.proc.wait(timeout=5)
        except (OSError, subprocess.SubprocessError):
            clean = False
        finally:
            self.reader.join(timeout=3)
            if self.reader.is_alive():
                try:
                    os.killpg(self.proc.pid, signal.SIGKILL)
                except OSError:
                    clean = False
                self.reader.join(timeout=3)
                clean &= not self.reader.is_alive()
            for stream in (self.proc.stdin, self.proc.stdout):
                try:
                    stream.close()
                except OSError:
                    clean = False
            self.h.ACTIVE.discard(self.proc)
        return clean

    def take_fetch_observations(self):
        if not self.trace:
            return []
        trace = self.trace
        self.trace = None
        try:
            records = [json.loads(line) for line in self.h.project_bytes(trace.parent, trace.name).decode().splitlines()]
            for record in records:
                if (not isinstance(record, dict) or set(record) - {'destination', 'host', 'path', 'method', 'status', 'transport_error'}
                        or record.get('destination') not in {'configured_loopback_gateway', 'denied_other_origin'}
                        or record.get('path') not in {'/v1/chat/completions', '/v1/responses', '/v1/responses/compact', '/v1/models', 'other'}
                        or record.get('method') not in {'GET', 'POST', 'HEAD', 'OPTIONS'}
                        or not (record.get('host') in {'127.0.0.1', 'api.openai.com', 'api.anthropic.com', 'server.codeium.com'} or re.fullmatch(r'[a-f0-9]{64}', str(record.get('host'))))):
                    raise ValueError('invalid_trace')
            return records
        except (ValueError, OSError, UnicodeDecodeError):
            self.trace_error = 'fetch_observation_unavailable_or_invalid'
            return []


def run(args, directory, session_hash, nonce, helpers):
    result = {'tier': 'client_mock' if args.upstream=='synthetic' else 'product_live_partial', 'client': 'omp', 'context_pressure': 'not_run', 'compact': 'not_run',
              'fork': 'not_run', 'disconnect_recovery': 'not_run', 'idle_process_recovery': 'not_run', 'worker': 'not_run', 'stage': 'rpc_start', 'status': 'failed'}
    rpc = None
    fetch_observations = []
    try:
        helpers.prepare_pressure(directory,nonce)
        rpc = Rpc(args, directory, helpers)
        state = rpc.call('get_state')
        if helpers.digest(state['sessionId'].encode()) != session_hash:
            raise ValueError('wrong_reopened_session')
        result['stage'] = 'fork'
        original = rpc.call('get_entries')['entries']
        message_ids = {entry['id'] for entry in original if entry.get('type') == 'message'}
        fork = rpc.call('fork')
        fork_state = rpc.call('get_state')
        fork_entries = rpc.call('get_entries')['entries']
        copied_ids = {entry['id'] for entry in fork_entries if entry.get('type') == 'message'}
        if fork.get('cancelled') is not False or fork_state['sessionId'] == state['sessionId'] or not message_ids or not message_ids.issubset(copied_ids):
            raise ValueError('fork_not_independent_or_history_missing')
        result.update(fork='passed', distinct_fork_session=True, original_history_copied=True)
        result['stage'] = 'idle_process_recovery'
        fetch_observations.extend(rpc.take_fetch_observations())
        if not rpc.close(abrupt=True):
            raise ValueError('rpc_cleanup_failed')
        rpc = Rpc(args, directory, helpers)
        reopened = rpc.call('get_state')
        entries = rpc.call('get_entries')['entries']
        restored_ids = {entry['id'] for entry in entries if entry.get('type') == 'message'}
        if reopened['sessionId'] != fork_state['sessionId'] or not message_ids.issubset(restored_ids):
            raise ValueError('session_changed_after_kill')
        result.update(idle_process_recovery='passed', disconnect_scope='idle process SIGKILL; not in-flight network failure', project_history_restored=True)
        result['stage'] = 'context_pressure'
        rpc.call('prompt', message='Exercise context pressure: use read to read every file context/part-00.txt through context/part-15.txt. Keep the shared CONTEXT_NONCE in memory. Then reply with exactly CONTEXT_READY.')
        if rpc.reads != 16 or rpc.final_text.strip() != 'CONTEXT_READY':
            raise ValueError('context_pressure_not_completed')
        result.update(context_pressure='passed', pressure_reads=rpc.reads, pressure_bytes=sum(len(helpers.project_bytes(directory/'workspace',f'context/part-{index:02d}.txt')) for index in range(16)))
        if os.environ.get('DEVIN_JOURNEY_CAPABILITIES') == 'independent':
            result.update(status='partial', stage='completed_independent_probes', compact='not_run',
                          compact_scope='separate compact probe failed; not retried in this independent run')
            return result
        result['stage'] = 'compact'
        prior_checkpoints = {entry['id'] for entry in rpc.call('get_entries')['entries'] if entry.get('type') == 'compaction'}
        style = os.environ.get('DEVIN_JOURNEY_COMPACT_STYLE', 'rpc')
        if style == 'soft':
            completed = rpc.call('prompt', message='/compact soft', local_prompt=True)
        elif style == 'rpc':
            rpc.call('compact')
            completed = {'local_command_acknowledged': True}
        else:
            raise ValueError('invalid_compact_style')
        # Local slash commands acknowledge enqueueing, while default compact
        # RPC waits for completion. Observe that same operation, never resend it.
        compact_deadline = min(rpc.deadline, time.monotonic() + 90)
        state = rpc.call('get_state')
        while state.get('isCompacting') is True and time.monotonic() < compact_deadline:
            time.sleep(.1)
            state = rpc.call('get_state')
        entries = rpc.call('get_entries')['entries']
        compact = next((entry for entry in reversed(entries) if entry.get('type') == 'compaction'), {})
        result['compact_checks'] = {'local_acknowledged': completed.get('local_command_acknowledged') is True,
                                    'new_checkpoint': bool(compact.get('id')) and compact.get('id') not in prior_checkpoints,
                                    'summary_present': bool(compact.get('summary')), 'nonce_preserved': nonce in compact.get('summary', ''),
                                    'is_compacting': state.get('isCompacting')}
        if (completed.get('local_command_acknowledged') is not True or not compact.get('summary') or nonce not in compact['summary']
                or compact.get('id') in prior_checkpoints or state.get('isCompacting') is not False):
            raise ValueError('compaction_not_persisted_or_lost_fact')
        if style == 'soft':
            rpc.control.end(True)
        result.update(compact='passed', summary_preserved_nonce=True, compaction_entry_observed=True,
                      compaction_tokens_before=compact.get('tokensBefore'), compact_mode='default compact RPC' if style == 'rpc' else 'explicit /compact soft',
                      usage_basis='synthetic input bytes/4; not provider usage' if args.upstream=='synthetic' else 'real usage must be verified in separate private execution records')
        result['stage'] = 'idle_process_recovery'
        # Kill the actual idle client after compaction, then reopen the same
        # persisted session. This does not claim an in-flight network recovery.
        fetch_observations.extend(rpc.take_fetch_observations())
        if not rpc.close(abrupt=True):
            raise ValueError('rpc_cleanup_failed')
        rpc = Rpc(args, directory, helpers)
        state = rpc.call('get_state')
        if state['sessionId'] != fork_state['sessionId']:
            raise ValueError('session_changed_after_kill')
        entries = rpc.call('get_entries')['entries']
        if not any(entry.get('type') == 'compaction' for entry in entries):
            raise ValueError('checkpoint_lost_after_kill')
        rpc.call('prompt', message='Recall the shared CONTEXT_NONCE from the compacted checkpoint without calling tools. Reply with only that value.')
        if rpc.reads != 0 or rpc.final_text.strip() != nonce:
            raise ValueError('compacted_fact_not_recalled')
        result.update(idle_process_recovery='passed', disconnect_scope='idle process SIGKILL; not in-flight network failure', checkpoint_restored=True, compacted_fact_recalled=True)
        result.update(status='passed', stage='completed')
    except (ValueError, OSError, KeyError, subprocess.SubprocessError) as error:
        allowed = {'rpc_process_exited', 'rpc_command_rejected', 'rpc_prompt_not_completed', 'rpc_event_failure', 'rpc_timeout',
                   'wrong_reopened_session', 'context_pressure_not_completed', 'compaction_not_persisted_or_lost_fact',
                   'session_changed_after_kill', 'checkpoint_lost_after_kill', 'compacted_fact_not_recalled', 'fork_not_independent_or_history_missing', 'rpc_cleanup_failed'}
        result['failure_kind'] = str(error) if isinstance(error, ValueError) and str(error) in allowed else 'local_probe_error'
        if result['stage'] in {'context_pressure', 'compact', 'fork', 'idle_process_recovery'}:
            result[result['stage']] = 'failed'
    finally:
        if rpc is not None:
            result.update(frames=rpc.frames, parse_errors=rpc.parse_errors)
            result.update(observed_reads=rpc.reads, context_ready_marker=rpc.final_text.strip() == 'CONTEXT_READY', event_failure=rpc.failed)
            result.update(error_categories=sorted(rpc.error_categories), error_http_status=rpc.error_http_status)
            result['error_vocabulary'] = sorted(rpc.error_vocabulary)
            try:
                fetch_observations.extend(rpc.take_fetch_observations())
                if fetch_observations:
                    result['fetch_observations'] = fetch_observations
                if getattr(rpc, 'trace_error', None):
                    result['diagnostic_error'] = rpc.trace_error
            except Exception:
                result['diagnostic_error'] = 'fetch_observation_unavailable_or_invalid'
            finally:
                result['cleanup_passed'] = rpc.close()
            if not result['cleanup_passed']:
                result['status'] = 'failed'
    return result
