package logger

// sinkguard.go：远端 sink（syslog / http）的公共防御边界——截断标记与归属名白名单净化。
// 两处后端对端均按「归属名」决定落盘文件名、按字节上限决定帧存活，防御语义同构，
// 故共享本文件的常量与函数；单帧预算常量仍留在各后端文件（syslogMaxFrame /
// syslogMaxFrameUDP / httpMaxFrame），因其数值语义与出处各异。

// truncMark 截断标记（syslog 截断后的 MSG 尾部、http 截断后的 message 尾部均追加，
// 其字节数计入各自单帧预算）。UTF-8 编码 14 字节：'…'=3B + "[truncated]"=11B。
const truncMark = "…[truncated]"

// sanitizeWhitelistName 把归属名收敛到对端落盘文件名白名单 [a-zA-Z0-9._-]：集合外
// 字节一律替换为 '-'（ASCII 白名单对 UTF-8 自同步，多字节字符逐字节替换为等量 '-'，
// 不产生非法 UTF-8）。空串回退 "-"，保证结果恒非空、确定性（替换非删除，仅输入为空
// 才可能为空）。
// 覆盖面（对端实测确证：http 帧 src 与 syslog 报文 HOSTNAME 位均决定落盘文件名，
// 含 "/.." 会 200/正常送达但文件名被污染，故两处对称防御）：
//   - http：newHTTPHandler 的 src 缺省链结果（Src > Service > os.Hostname()）；
//   - syslog：newSyslogHandler 的 hostname 缺省链结果（Hostname > Service > os.Hostname()）。
//
// syslog 的 APP-NAME / PROCID 不净化：对端落盘文件名只取 HOSTNAME 位（接入实测确证），
// 二者不决定文件名，属报文其余格式——维持原样以保守「不改既有报文格式」边界。
func sanitizeWhitelistName(name string) string {
	if name == "" {
		return "-"
	}
	b := []byte(name)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			b[i] = '-'
		}
	}
	return string(b)
}
