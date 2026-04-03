-- Seed rule strategies for testing
-- 名詞定義：入金/出金 = 法幣，入幣/提幣 = 加密貨幣

-- Rule 1: 單筆大額提幣 (單筆提幣 > 100,000)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '單筆大額提幣',
    '單筆加密貨幣提幣金額超過 100,000',
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

-- Rule 3: 大額入金後快速提幣 (提幣時，1 小時內有大額入金 > 50,000)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '大額入金後快速提幣',
    '提幣時，1 小時內曾有單筆入金超過 50,000',
    '{
        "type": "AND",
        "children": [
            {"type": "CONDITION", "field": "behavior", "operator": "=", "value": "CryptoWithdraw"},
            {
                "type": "CONDITION",
                "field": "FiatDeposit:COUNT",
                "operator": ">",
                "value": 0,
                "window": {"value": 1, "unit": "hours"}
            },
            {
                "type": "CONDITION",
                "field": "FiatDeposit:MAX:amount",
                "operator": ">",
                "value": 50000,
                "window": {"value": 1, "unit": "hours"}
            }
        ]
    }',
    1
);

-- Rule 4: 異常登入後提幣 (非常用國家登入後執行提幣)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '異常登入後提幣',
    '從非常用國家登入後執行加密貨幣提幣',
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

-- Rule 5: 分散小額提幣 (24 小時內提幣次數 > 10 且單筆 < 10,000)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '分散小額提幣',
    '24 小時內提幣超過 10 次且單筆金額低於 10,000，疑似拆分洗錢',
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

-- Rule 6: 累積大額提幣 (7 天內提幣總額 > 500,000)
INSERT INTO rule_strategies (name, description, rule_node, status) VALUES (
    '累積大額提幣',
    '7 天內加密貨幣提幣總額超過 500,000',
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
