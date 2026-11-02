"""Explicit operator-plan facts, never a global production price fallback.

The user confirmed SWE-2 is free on 2026-10-25. The observed native grant used
in this task is exactly swe-2-high. No other UID or paid Fusion member inherits
this fact, and no official provider price card is claimed.
"""


def user_confirmed_swe2_rule(model_uid, discovered_uids):
    if model_uid!='swe-2-high' or model_uid not in set(discovered_uids):
        raise ValueError('confirmed_free_price_requires_exact_discovered_swe2_uid')
    rule={'platform':'devin','models':[model_uid],'billing_mode':'token',
          'input_price':0,'output_price':0,'cache_write_price':0,'cache_read_price':0}
    provenance={'source':'user_confirmed','statement_date':'2026-10-25','model_uid':model_uid,
                'uid_mapping_basis':'exact UID present in authenticated native catalog',
                'official_price_card_verified':False,'production_configuration_changed':False}
    return rule,provenance
