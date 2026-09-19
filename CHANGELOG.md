# Changelog

## 修复：多字节空值位图、满长字段与 nullable 标志三处边界缺陷

### 缺陷现象

dBase 把 Varchar/Varbinary 列的“变长标志”和“空值标志”压进行尾隐藏的
`_NullFlags` 位图列：每个变长列占 1 个变长位，可空列再占 1 个空值位。
三处共享根因都出在行编码（`Row.ToBytes`）与建表（`NewTable`）对这张位图
的处理上：

1. **多字节空值位图**：`NewTable` 用 `nullFlagLength / 8` 截断取整计算
   `_NullFlags` 列宽（10 个状态位只分到 1 字节），`Row.ToBytes` 又把位图
   硬编码为 `make([]byte, 1)`。五个可空变长列（10 个状态位）时，写路径
   `nullFlag[1]` 越界 panic，读路径 `getNthBit` 在 1 字节缓冲上取第 8 位
   同样越界 panic。
2. **位位置错位**：`ToBytes` 只在列可空时推进位游标 `varPos++`，漏掉了
   每列必有的变长位，而读路径 `nullFlagPosition` 按“每列 1 位 + 可空再
   1 位”计数。第二张及以后的变长列全部读写错位，短值被读成满长、null
   被读成空串。空值位跨字节时（`bitIndex == 7`）旧代码对单字节执行
   `setNthBit(b, 8)`，`byte(1) << 8 == 0`，空值位被静默丢弃。
3. **nullable 标志与空值语义**：旧代码对 `length == 0` 一律写空值位，
   不区分 nullable——非可空列根本没有空值位，写下去会污染下一列的变长
   位；空字符串 `""` 与 null（读路径产生的 `[]byte{}`/`nil`）被压成同一
   编码，往返后空串丢失成 null。

### 实现选择

- `NewTable`（`dbase/table.go`）：恢复向上取整
  `length = nullFlagLength/8; if nullFlagLength%8 > 0 { length++ }`，
  保证 `_NullFlags` 列宽足以容纳全部状态位。
- `Row.ToBytes`（`dbase/table.go`）：
  - 位图按 `nullFlagColumn.Length` 预分配，并通过 `setNullFlagBit` 闭包
    按需增长，任何列数下都不会越界；
  - 恢复每个变长列 `varPos++`、可空列再 `varPos++`，与读路径
    `nullFlagPosition` 的计数严格对齐；
  - 空值位按绝对位序 `varPos+1` 计算字节/位下标，跨字节不再丢失；
  - 依据字段值的 Go 类型区分 null 与空值：`nil` 或空 `[]byte`（读路径
    对 null 的表示）视为 null，字符串（含 `""`）视为非空值；空值位只在
    列带 `NullableFlag` 时写入，非可空列的空标记降级为“长度 0 的变长
    值”（变长位置位 + 长度字节 0），不再污染相邻列的位；
  - 值长度超过列宽时返回带列名与两个长度的 `NewErrorf` 错误，替代原先
    `copy` 的静默截断，保留可诊断上下文。
- `getNthBit`（`dbase/conversion.go`）：边界条件由 `n > len*8` 改为
  `n < 0 || n >= len*8`，对截断/损坏的位图返回 false 而不是 panic。

### 原覆盖的空白

原测试没有任何用例创建含 Varchar/Varbinary 的表：`ToBytes` 的位图分支、
`NewTable` 的 `_NullFlags` 列宽计算、`ReadNullFlag` 的多字节读取全部零
覆盖，因此“位图超过一个字节”这一整类行为（≥5 个可空变长列）从未被执
行过，空串与 null 的区分也无从谈起。

### 回归用例（`dbase/table_nullflag_test.go`）

- `TestRowToBytesNullFlagMultiByte`：5 个可空 Varchar（10 个状态位）+
  1 个非可空 Varbinary + 1 个普通 Character 列，共 11 个状态位、2 字节
  位图。直接断言 `ToBytes` 不再 panic、位图两字节为 `0xA5 0x04`、删除
  位（0x20/0x2A）保持不变、位图之后的普通列偏移（第 59 字节起）不变。
- `TestRowToBytesNullFlagByteBoundary`：4 个可空 Varchar，第四列的空值
  位恰好是位图第 7 位，锁定空值位跨字节写入不再被静默丢弃。
- `TestNullableVarcharRoundTrip`：三行 null、空串、短值、满长值交错
  （含满长 Varbinary 与非可空列 nil）写盘重开后逐列比对，要求每列的
  null、短值、满长值与原输入完全一致。

### 相邻语义的退化保护

- 删除位与列偏移：位图仍追加在行尾，删除标志仍在字节 0，普通列偏移由
  `header.RowLength` 与列定义决定，回归用例对三者都有字面断言。
- 非可空变长列：空标记降级为长度 0 的变长值，读回得到空串/空切片，
  不占用也不污染任何其他列的位。
- 满长字段：长度恰等于列宽时不写任何状态位，读回为完整列宽值；超过
  列宽现在报错而不是静默截断。
- `getNthBit` 的既有表驱动断言（含 `out of range`、`multiple bytes`）
  原样保留并通过；存量测试未删改任何断言。

### 最危险反例

最危险的反例是 **“5 个可空 Varchar 列、第 4 列为 null”**：10 个状态位
需要 2 字节位图，而第 4 列的空值位恰是位图第 7 位——它同时触发全部三
处缺陷：建表时位图列宽被截断成 1 字节、写路径位游标错位且位图只有 1
字节（越界 panic）、空值位 `setNthBit(b, 8)` 静默丢失。即便绕过 panic，
读回时该 null 会变成空串、其后所有列的状态位整体错位，属于“写时不报
错、读回静默错乱”的损坏。它由 `TestRowToBytesNullFlagByteBoundary`
（锁第 7 位空值位）与 `TestRowToBytesNullFlagMultiByte`（锁多字节位图
整体布局）两个回归用例共同覆盖。

### 已知无关失败

`TestOpenDatabase*` / `TestDatabaseTableAccess` / `TestDatabaseClose` 在
未修改的原始检出上同样失败：`examples/test_data/database/EXPENSES.DBC`
引用的 `employees.DBF` 未随仓库提供，属于既有 fixture 缺口，与本次修改
无关。
