"""Code worker task implementations.

All tasks receive (task_id, payload: str, reporter) and return a JSON-serializable dict.
Real work happens here — no mocks.
"""
import ast
import re
import time


def _parse_or_none(src: str):
    try:
        return ast.parse(src)
    except SyntaxError as e:
        return e


def format_code(task_id: str, payload: str, reporter) -> dict:
    """Reformat Python source code using ast.parse + ast.unparse.

    For code that can't be parsed, fall back to whitespace normalization.
    """
    reporter.progress(10, "[format] 解析 AST")
    time.sleep(0.2)
    tree = _parse_or_none(payload)
    if isinstance(tree, ast.AST):
        reporter.progress(40, "[format] AST 解析成功，开始 unparse")
        # ast.unparse preserves semantics and adds reasonable spacing
        formatted = ast.unparse(tree)
        reporter.progress(70, "[format] 应用额外格式化（缩进/空行）")
        # Add two blank lines between top-level def/class
        lines = formatted.split("\n")
        out = []
        prev_top = False
        for line in lines:
            cur_top = bool(re.match(r"^(def |class |async def )", line))
            if cur_top and prev_top and out and out[-1].strip() != "":
                out.append("")
                out.append("")
            out.append(line)
            prev_top = cur_top
        formatted = "\n".join(out) + "\n"
        reporter.progress(95, "[format] 完成")
        return {
            "success": True,
            "method": "ast.unparse",
            "original_lines": len(payload.splitlines()),
            "formatted_lines": len(formatted.splitlines()),
            "code": formatted,
        }
    else:
        # Fallback: normalize whitespace and indentation heuristically
        reporter.progress(40, "[format] AST 解析失败，使用启发式格式化")
        text = payload.replace("\r\n", "\n").replace("\t", "    ")
        # Collapse multiple blank lines
        text = re.sub(r"\n{3,}", "\n\n", text)
        reporter.progress(95, "[format] 完成（启发式）")
        return {
            "success": True,
            "method": "heuristic",
            "warning": f"语法错误，无法用 AST 格式化：{tree.msg if hasattr(tree, 'msg') else tree}",
            "code": text,
        }


def count_ast(task_id: str, payload: str, reporter) -> dict:
    """Count functions, classes, loops, branches, and max nesting depth."""
    reporter.progress(15, "[ast] 解析")
    time.sleep(0.2)
    tree = _parse_or_none(payload)
    if isinstance(tree, SyntaxError):
        return {"success": False, "error": f"语法错误: {tree.msg} (line {tree.lineno})"}

    counts = {
        "function_def": 0,
        "async_function_def": 0,
        "class_def": 0,
        "for_loop": 0,
        "while_loop": 0,
        "if_branch": 0,
        "try_block": 0,
        "import": 0,
        "total_nodes": 0,
    }

    max_depth = [0]

    class Visitor(ast.NodeVisitor):
        def __init__(self):
            self.depth = 0

        def generic_visit(self, node):
            counts["total_nodes"] += 1
            self.depth += 1
            if self.depth > max_depth[0]:
                max_depth[0] = self.depth
            self._check(node)
            super().generic_visit(node)
            self.depth -= 1

        def _check(self, node):
            if isinstance(node, ast.FunctionDef):
                counts["function_def"] += 1
            elif isinstance(node, ast.AsyncFunctionDef):
                counts["async_function_def"] += 1
            elif isinstance(node, ast.ClassDef):
                counts["class_def"] += 1
            elif isinstance(node, (ast.For, ast.AsyncFor)):
                counts["for_loop"] += 1
            elif isinstance(node, ast.While):
                counts["while_loop"] += 1
            elif isinstance(node, ast.If):
                counts["if_branch"] += 1
            elif isinstance(node, ast.Try):
                counts["try_block"] += 1
            elif isinstance(node, (ast.Import, ast.ImportFrom)):
                counts["import"] += 1

    reporter.progress(50, "[ast] 遍历 AST")
    time.sleep(0.3)
    Visitor().visit(tree)
    reporter.progress(90, "[ast] 汇总")
    time.sleep(0.1)
    counts["max_nesting_depth"] = max_depth[0]
    counts["source_lines"] = len(payload.splitlines())
    counts["source_chars"] = len(payload)
    return {"success": True, "counts": counts}
