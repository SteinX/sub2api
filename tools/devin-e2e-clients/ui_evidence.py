"""Bounded, structural UI progress survives an outer process timeout."""
import json

STAGES=frozenset({'browser-launch','navigate','product-login','product-key-list','product-key-modal','model-select-control',
                 'catalog-loading','catalog-timeout','catalog-refresh','model-target-option','client-tab','environment-key',
                 'config-generation','clipboard','clipboard-copied','model-target-ready','browser-close','browser-closed'})
COUNTS={'elapsed_ms','catalog_requests','catalog_responses','copied_files','catalog_ordinal'}
FIELDS={'ui_progress','stage','failure_stage','catalog_event','http_status'}|COUNTS


def parse_ui_output(raw,partial=False):
    if not isinstance(raw,bytes) or len(raw)>1024*1024:
        raise ValueError('invalid_ui_output_bound')
    terminal=progress=None
    observations=[]
    lines=raw.splitlines()
    for index,line in enumerate(lines):
        try: frame=json.loads(line)
        except (ValueError,UnicodeDecodeError):
            if partial and index==len(lines)-1:break
            raise ValueError('invalid_ui_frame') from None
        if not isinstance(frame,dict):raise ValueError('invalid_ui_frame')
        if frame.get('ui_progress') is True:
            if (set(frame)!=FIELDS or frame.get('stage') not in STAGES
                    or frame.get('failure_stage') not in STAGES|{None}
                    or frame.get('catalog_event') not in {'stage','request','headers','body-finished','request-failed'}
                    or not (frame.get('http_status') is None or type(frame['http_status']) is int and 100<=frame['http_status']<=599)
                    or any(type(frame.get(k)) is not int or not 0<=frame[k]<=10**9 for k in COUNTS)):
                raise ValueError('invalid_ui_progress')
            progress=frame
            observations.append(frame)
        else:
            if terminal is not None or type(frame.get('passed')) is not bool:raise ValueError('invalid_ui_terminal')
            terminal=frame
    if terminal is None and not partial:raise ValueError('ui_terminal_missing')
    if progress is not None:progress={**progress,'observations':observations[-128:],'observations_truncated':len(observations)>128}
    return terminal,progress
