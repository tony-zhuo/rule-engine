-- Seed rule strategies for testing

-- Rule 1: 單筆大額提款 (單筆提款 > 100,000)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '單筆大額提款',
    '單筆加密貨幣提款金額超過 100,000',
    '{
        "type": "AND",
        "children": [
            {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoWithdraw"},
            {"type": "CONDITION", "field": "amount", "operator": ">", "value": 100000}
        ]
    }',
    1
);

-- Rule 2: 高頻交易 (3 天內交易次數 > 50)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '高頻交易',
    '3 天內交易次數超過 50 次',
    '{
        "type": "CONDITION",
        "field": "Trade:COUNT",
        "operator": ">",
        "value": 50,
        "window": {"value": 3, "unit": "days"}
    }',
    1
);

-- Rule 3: 大額入金後快速出金 (入金 > 50,000 且 1 小時內出金 > 1 次)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '大額入金後快速出金',
    '單筆入金超過 50,000 且 1 小時內有出金行為',
    '{
        "type": "AND",
        "children": [
            {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "FiatDeposit"},
            {"type": "CONDITION", "field": "amount", "operator": ">", "value": 50000},
            {
                "type": "CONDITION",
                "field": "CryptoWithdraw:COUNT",
                "operator": ">",
                "value": 0,
                "window": {"value": 1, "unit": "hours"}
            }
        ]
    }',
    1
);

-- Rule 4: 異常登入 + 提款 (非常用國家登入且有提款行為)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '異常登入後提款',
    '從非常��國家登入後執行提款',
    '{
        "type": "AND",
        "children": [
            {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoWithdraw"},
            {
                "type": "CONDITION",
                "field": "Login:COUNT",
                "operator": ">",
                "value": 0,
                "window": {"value": 30, "unit": "minutes"}
            },
            {"type": "CONDITION", "field": "country", "operator": "NOT_IN", "value": ["TW", "JP", "US"]}
        ]
    }',
    1
);

-- Rule 5: 分散小額出金 (24 小時內出金次數 > 10 且單筆 < 10,000)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '分散小額出金',
    '24 小時內出金超過 10 次且單筆金額低於 10,000，疑似拆分洗錢',
    '{
        "type": "AND",
        "children": [
            {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoWithdraw"},
            {"type": "CONDITION", "field": "amount", "operator": "<", "value": 10000},
            {
                "type": "CONDITION",
                "field": "CryptoWithdraw:COUNT",
                "operator": ">",
                "value": 10,
                "window": {"value": 24, "unit": "hours"}
            }
        ]
    }',
    1
);

-- Rule 6: 累積大額出金 (7 天內出金總額 > 500,000)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '累積大額出金',
    '7 天內加密貨幣出金總額超過 500,000',
    '{
        "type": "CONDITION",
        "field": "CryptoWithdraw:SUM:amount",
        "operator": ">",
        "value": 500000,
        "window": {"value": 7, "unit": "days"}
    }',
    1
);

-- Rule 7: 新帳號大額操作 (註冊 < 24 小時且交易金額 > 50,000) — 停用狀態
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '新帳號大額操作',
    '新註冊帳號進行大額交易（已停用）',
    '{
        "type": "AND",
        "children": [
            {"type": "CONDITION", "field": "account_age_hours", "operator": "<", "value": 24},
            {"type": "CONDITION", "field": "amount", "operator": ">", "value": 50000}
        ]
    }',
    2
);
