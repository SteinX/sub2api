"""Bounded, structural UI progress survives an outer process timeout."""
import json
import subprocess
import time

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


def communicate_ui(process,input_data,*,upstream,catalog_recovery):
    """One process, no resend: bound mock cold start independently of UI work.

    The recovery probe deliberately consumes 16s after the modal opens. CI
    evidence showed 39.6s of browser setup consuming its prior entire 60s cap.
    Real/ordinary paths keep 60s. The mock probe allows startup<=45s, UI<=60s,
    absolute process<=85s, and remains inside the existing Go 90s case bound.
    """
    if upstream!='synthetic' or not catalog_recovery:
        return process.communicate(input_data,timeout=60)
    started=time.monotonic()
    try:
        return process.communicate(input_data,timeout=45)
    except subprocess.TimeoutExpired as error:
        try: _,progress=parse_ui_output(error.output or b'',partial=True)
        except ValueError: raise error from None
        observations=(progress or {}).get('observations',[])
        ready=next((frame['elapsed_ms']/1000 for frame in observations if frame['stage']=='navigate'),None)
        elapsed=time.monotonic()-started
        if ready is None or ready>45 or ready>elapsed or (progress or {}).get('observations_truncated'):
            raise
        remaining=min(85-elapsed,60-(elapsed-ready))
        if remaining<=0:raise
        # Continue reading the same process; never resend stdin, restart the
        # browser, click refresh again, authenticate or issue a model request.
        return process.communicate(timeout=remaining)
