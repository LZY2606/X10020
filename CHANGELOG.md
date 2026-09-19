# Changelog

## 修复：多字节空值位图、满长字段与 nullable 标志三处边界缺陷

### 问题

dBase 的 Varchar/Varbinary 列把"长度标志"和"空值标志"压进行尾隐藏的
`_NullFlags` 位图列：每个变长列占 1 个长度位，可空列再占 1 个空值位。
列数较少时所有状态位都落在第一个字节里，三处边界缺陷因此被掩盖：

1. **多字节空值位图**：`Row.ToBytes` 把位图硬编码为 `make([]byte, 1)`，
   而 `New` 创建 `_NullFlags` 列时长度按 `nullFlagLength / 8` 向下取整。
   五个可空变长列（10 个状态位）时，写入侧 `nullFlag[1]` 越界 panic；
   读取侧 `getNthBit` 的边界判断 `n > len*8` 差一，访问位图末尾后一位时
   同样越界 panic。
2. **nullable 标志**：`ToBytes` 推进位图游标时漏掉了长度位（`varPos++`），
   只有可空列才推进 1 位，导致第二个变长列起所有标志位整体漂移；
   且空值位按 `bitIndex+1` 写入，当长度位落在字节第 7 位时，
   `setNthBit(b, 8)` 的单字节移位 `byte(1) << 8` 静默得 0，空值位被丢弃。
3. **满长字段 / 空字符串 / null 往返丢失**：长度恰为 0 的值一律被写成
   null，空字符串 `""` 读回变成 `[]byte{}`；nil 值经 `Represent` 变成
   全零满长字段；非可空列的空值位会写到相邻列的长度位上。

### 实现选择

- `dbase/table.go` `New`：`_NullFlags` 列长度改为 `(nullFlagLength+7)/8`
  向上取整，保证所有状态位有完整字节容纳。
- `dbase/table.go` `Row.ToBytes`：
  - 位图按列定义动态计算位数（每变长列 1 位 + 每可空列 1 位）并分配
    `(bits+7)/8` 字节，不再硬编码 1 字节；
  - 每个变长列先推进长度位游标，可空列再推进空值位游标，与读取侧
    `nullFlagPosition` 的计数方式严格对齐；
  - 空值位按绝对位号 `varPos+1` 取 `（位号/8, 位号%8)` 写入，跨字节边界
    时正确落到下一字节的第 0 位；
  - null 判定改为看字段值本身：nil 或空的非字符串值（`[]byte{}`，即
    读取侧 null 的表示）才算 null；空字符串是合法值，写为"变长 + 长度
    0"，与 null 区分；空值位只在列可空时设置，非可空列的空值降级为
    变长空值，不再污染相邻列的标志位。
- `dbase/conversion.go` `getNthBit`：越界判断改为 `n < 0 || n >= len*8`，
  位图末尾后一位返回 false 而不是 panic。

所有入口共享同一条编码路径（`ToBytes` 被 `WriteRow` 等各 IO 实现调用）
和解码路径（`ReadNullFlag`/`getNthBit`），因此修复一次即在全部入口生效。
错误仍通过 `WrapError`/`NewErrorf` 保留列名、位置等可诊断上下文，未新增
任何样例字面量特判。

### 原覆盖的空白

原测试只有读固定 fixture 的用例，没有任何"建表 → 写行 → 读回"的往返
用例；`getNthBit` 只测了字节内位和明显越界位，没测恰好等于位图末尾的
边界位；`_NullFlags` 列长度、位图字节内容、空字符串与 null 的区别完全
没有断言。

### 相邻语义的退化保护

新增 `dbase/nullflag_test.go`，每个现象可单独定位：

- `TestNullFlagColumnLengthRounding`：1/4/5/8/9 个可空列与 8/9 个非可空
  列的 `_NullFlags` 长度，锁定向上取整边界（恰好填满整字节时不加一）。
- `TestRowToBytesNullFlagMultiByte`：五个可空变长列（10 个状态位）不再
  panic，并逐字节断言位图内容（`0x19 0x01`）与满长字段原样落盘。
- `TestVarcharNullFlagRoundTrip`：六个变长列（11 个状态位，中间夹一个
  非可空列）+ 后续普通列，四行覆盖 null、空字符串、短值、满长值交错，
  写盘后重开逐字段比对；同时断言删除标志位（0x20/0x2A）与后续普通列
  的原始偏移不变。
- `TestVarcharEmptyStringVsNull`：单可空列内 `""`、`nil`、短值、满长值
  的往返，锁定空字符串与 null 的可区分性。
- `TestGetNthBitBoundary`：位图末尾后一位返回 false，末位本身可读。

### 最危险反例

最危险的反例是**第 8 个状态位上的可空列**：当某变长列的长度位恰好落在
位图字节的第 7 位（如 5 个可空列布局中第 4 个可空列 V4，位号 7）时，
其空值位是位号 8。旧代码 `setNthBit(b, 8)` 的单字节移位静默得 0，null
被写成"满长全零字段"，读回变成 8 个 `\x00` 的字符串——不 panic、不
报错，数据静默损坏，且只有列数足够多、状态位跨字节时才触发。它由
`TestVarcharNullFlagRoundTrip`（V4 列在 row 1 中为 null，断言读回
`[]byte{}`）和 `TestRowToBytesNullFlagMultiByte`（断言第二位图字节的
确切内容）共同锁定。
