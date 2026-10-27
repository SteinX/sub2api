"""Track the latest CLI terminal; text is diagnostic, never project proof."""
import hashlib


class CompletionEvidence:
    def __init__(self):
        self.parts = {}
        self.terminal_seen = False
        self.normal_stop = False
        self.failed = False
        self.anonymous_parts = 0
        self.active_phase = None
        self.marker_seen_anywhere = False

    def begin(self, clear_text=False):
        self.terminal_seen = self.normal_stop = False
        if clear_text:
            self.parts = {}

    def observe(self, event):
        kind = event.get('type')
        if kind == 'step_start':
            self.begin(clear_text=True)
            self.active_phase = 'step'
        elif kind in {'turn_start', 'agent_start'}:
            self.begin(clear_text=True)
            self.active_phase = None
        elif kind == 'message_start' and event.get('message', {}).get('role') == 'assistant':
            self.begin(clear_text=True)
            self.active_phase = 'assistant'
        elif kind in {'tool_use', 'tool_execution_start', 'tool_execution_end'}:
            self.begin()
        elif kind == 'error':
            self.failed = True
            self.begin()
            self.active_phase = None
        elif kind == 'text':
            part = event.get('part', {})
            text = part.get('text')
            if not isinstance(text, str):
                raise ValueError('invalid_text_event')
            self.begin()
            key = part.get('id')
            if not isinstance(key, str) or not key:
                self.anonymous_parts += 1
                key = ('anonymous', self.anonymous_parts)
            # OpenCode text parts with an ID are snapshots. Anonymous fragments
            # are appended; neither representation can carry an old terminal.
            self.parts[key] = text
            self.marker_seen_anywhere |= any(marker in text for marker in ['PROJECT_COMPLETE', 'PROJECT_REVIEWED'])
        elif kind == 'step_finish':
            if self.active_phase != 'step':
                raise ValueError('step_terminal_without_start')
            self.terminal_seen = True
            self.normal_stop = event.get('part', {}).get('reason') in {'stop', 'end_turn'}
            self.active_phase = None
        elif kind == 'message_end' and event.get('message', {}).get('role') == 'assistant':
            if self.active_phase != 'assistant':
                raise ValueError('assistant_terminal_without_start')
            message = event['message']
            self.parts = {'final_assistant': ''.join(p.get('text', '') for p in message.get('content', []) if p.get('type') == 'text')}
            reason = message.get('stopReason')
            self.terminal_seen = True
            self.normal_stop = reason in {'stop', 'end_turn'}
            self.failed |= reason in {'error', 'aborted'}
            self.marker_seen_anywhere |= any(marker in self.parts['final_assistant'] for marker in ['PROJECT_COMPLETE', 'PROJECT_REVIEWED'])
            self.active_phase = None

    def report(self, expected_marker):
        text = ''.join(self.parts.values()).strip().encode()
        return {'completion_contract': 'project_artifacts_v2', 'terminal_event_seen': self.terminal_seen,
                'normal_stop': self.normal_stop, 'final_text_bytes': len(text),
                'final_text_sha256': hashlib.sha256(text).hexdigest(), 'final_text_parts': len(self.parts),
                'marker_seen_anywhere': self.marker_seen_anywhere,
                'final_text_matches': text == expected_marker.encode()}
