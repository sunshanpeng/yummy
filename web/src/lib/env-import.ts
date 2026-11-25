export type EnvEntry = { name: string; value: string };

export function parseEnv(input: string): EnvEntry[] {
  const lines = input.replace(/^\uFEFF/, "").split(/\r\n|\r|\n/);
  const entries: EnvEntry[] = [];
  const names = new Set<string>();
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trimStart();
    if (!line || line.startsWith("#")) continue;
    const lineNumber = i + 1;
    const match = /^(?:export\s+)?([A-Za-z_][A-Za-z_0-9]{0,127})\s*=\s*(.*)$/.exec(line);
    if (!match) throw new Error(`第 ${lineNumber} 行：请输入合法的变量名和等号。`);
    const [, name, initial] = match;
    if (names.has(name)) throw new Error(`第 ${lineNumber} 行：变量名称重复。`);

    const quote = initial[0];
    let value: string;
    if (quote === '"' || quote === "'" || quote === "`") {
      let rest = initial.slice(1);
      const closingQuote = quote === '"' ? /^(?:[^"\\]|\\[^\n])*"/ : quote === "'" ? /^[^']*'/ : /^[^`]*`/;
      const parts: string[] = [];
      let closing = closingQuote.exec(rest);
      while (!closing && i + 1 < lines.length) {
        parts.push(rest);
        rest = lines[++i];
        closing = closingQuote.exec(rest);
      }
      if (!closing) throw new Error(`第 ${lineNumber} 行：引号未闭合。`);
      if (!/^\s*(?:#.*)?$/.test(rest.slice(closing[0].length))) {
        throw new Error(`第 ${lineNumber} 行：引号后仅允许空白和注释。`);
      }
      parts.push(closing[0].slice(0, -1));
      value = parts.join("\n");
      if (quote === '"') {
        const escaped: Record<string, string> = { n: "\n", r: "\r", t: "\t", '"': '"', "\\": "\\" };
        value = value.replace(/\\([nrt"\\])/g, (_, char: string) => escaped[char]);
      }
    } else {
      value = initial.split("#", 1)[0].trim();
    }
    names.add(name);
    entries.push({ name, value });
  }
  return entries;
}
