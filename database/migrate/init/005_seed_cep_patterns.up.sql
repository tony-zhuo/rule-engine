-- Seed CEP patterns for testing (ordered event sequences)

-- CEP 1: 入金 → 交易 TWD → 提幣（20 分鐘內）
INSERT INTO cep_patterns (id, name, description, states, status) VALUES (
    'fiat-trade-withdraw',
    '入金後交易再提幣',
    '會員在 20 分鐘內依序完成入金、TWD 交易（≥50,000）、提幣',
    '[
        {
            "name": "入金",
            "condition": {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "FiatDeposit"},
            "max_wait": {"value": 20, "unit": "minutes"},
            "context_binding": {"deposit_amount": "$event.amount"}
        },
        {
            "name": "交易TWD",
            "condition": {
                "type": "AND",
                "children": [
                    {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "Trade"},
                    {"type": "CONDITION", "field": "currency", "operator": "=", "value": "TWD"},
                    {"type": "CONDITION", "field": "amount", "operator": ">=", "value": 50000}
                ]
            },
            "max_wait": {"value": 20, "unit": "minutes"}
        },
        {
            "name": "提幣",
            "condition": {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoWithdraw"},
            "max_wait": {"value": 20, "unit": "minutes"}
        }
    ]',
    1
);

-- CEP 2: 異常登入 → 改密碼 → 提幣（30 分鐘內）
INSERT INTO cep_patterns (id, name, description, states, status) VALUES (
    'login-password-withdraw',
    '異常登入改密碼後提幣',
    '從非常用國家登入後修改密碼再提幣，疑似帳號被盜',
    '[
        {
            "name": "異常登入",
            "condition": {
                "type": "AND",
                "children": [
                    {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "Login"},
                    {"type": "CONDITION", "field": "country", "operator": "NOT_IN", "value": ["TW", "JP", "US"]}
                ]
            },
            "max_wait": {"value": 30, "unit": "minutes"},
            "context_binding": {"login_country": "$event.country", "login_ip": "$event.ip"}
        },
        {
            "name": "改密碼",
            "condition": {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "ChangePassword"},
            "max_wait": {"value": 30, "unit": "minutes"}
        },
        {
            "name": "提幣",
            "condition": {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoWithdraw"},
            "max_wait": {"value": 30, "unit": "minutes"},
            "context_binding": {"withdraw_address": "$event.target_address"}
        }
    ]',
    1
);

-- CEP 3: 多筆小額入幣 → 大額提幣（1 小時內入幣 3 次後提幣）
INSERT INTO cep_patterns (id, name, description, states, status) VALUES (
    'multi-deposit-withdraw',
    '多筆入幣後大額提幣',
    '短時間內多次小額入幣後一次大額提幣，疑似混幣洗錢',
    '[
        {
            "name": "入幣1",
            "condition": {
                "type": "AND",
                "children": [
                    {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoDeposit"},
                    {"type": "CONDITION", "field": "amount", "operator": "<", "value": 10000}
                ]
            },
            "max_wait": {"value": 1, "unit": "hours"}
        },
        {
            "name": "入幣2",
            "condition": {
                "type": "AND",
                "children": [
                    {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoDeposit"},
                    {"type": "CONDITION", "field": "amount", "operator": "<", "value": 10000}
                ]
            },
            "max_wait": {"value": 1, "unit": "hours"}
        },
        {
            "name": "入幣3",
            "condition": {
                "type": "AND",
                "children": [
                    {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoDeposit"},
                    {"type": "CONDITION", "field": "amount", "operator": "<", "value": 10000}
                ]
            },
            "max_wait": {"value": 1, "unit": "hours"}
        },
        {
            "name": "大額提幣",
            "condition": {
                "type": "AND",
                "children": [
                    {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoWithdraw"},
                    {"type": "CONDITION", "field": "amount", "operator": ">=", "value": 20000}
                ]
            },
            "max_wait": {"value": 1, "unit": "hours"}
        }
    ]',
    1
);
