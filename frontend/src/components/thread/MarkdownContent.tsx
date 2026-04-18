import Markdown, { type Components } from "react-markdown";
import { Link } from "react-router";
import remarkGfm from "remark-gfm";
import { CitationChip } from "./CitationChip";

const REMARK_PLUGINS = [remarkGfm];

const LINK_CLASS = "text-[#007acc] underline-offset-2 hover:underline";

// Internal paths stay inside the SPA (react-router <Link>); anything else opens
// in a new tab. The model is instructed to emit `/doc/{id}?page={n}` for page
// citations — see DEFAULT_INSTRUCTIONS in src/constants.ts.
//
// Protocol-relative URLs like `//evil.com` also start with `/` but the browser
// resolves them as full external navigations, so they're explicitly rejected.
function isInternalHref(href: string | undefined): href is string {
  if (typeof href !== "string") return false;
  if (!href.startsWith("/")) return false;
  return !href.startsWith("//");
}

// The custom citation href scheme: the preprocessor rewrites
// `[display text][citation_id]` in the model's output to a markdown link
// with href `citation:citation_id`, so react-markdown sees a normal link
// and the `a` renderer below swaps it for a CitationChip. Non-matching
// hrefs fall through to the internal/external link handling above.
const CITATION_HREF_PREFIX = "citation:";

function parseCitationHref(href: string | undefined): number | null {
  if (typeof href !== "string") return null;
  if (!href.startsWith(CITATION_HREF_PREFIX)) return null;
  const raw = href.slice(CITATION_HREF_PREFIX.length);
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) return null;
  return parsed;
}

// `[display text][citation_id]` → `[display text](citation:citation_id)`.
// react-markdown's default handling of reference-style links treats
// undefined references as literal text, which would keep the bracket
// syntax in the rendered output. Rewriting to an inline link with a
// custom href scheme hooks into the `a` renderer below and produces a
// real CitationChip.
//
// The pattern deliberately only matches positive integers in the id
// slot so ordinary markdown reference-style links (which use text ids
// like `[example][1]` → `[1]: https://...`) still work when the
// definition exists below.
const CITATION_INLINE_PATTERN = /\[([^\]\n]+?)\]\[(\d+)\]/g;

function preprocessCitations(text: string): string {
  return text.replace(CITATION_INLINE_PATTERN, "[$1](citation:$2)");
}

const COMPONENTS: Components = {
  p: ({ children }) => <p className="m-0 mb-2 last:mb-0">{children}</p>,
  ul: ({ children }) => <ul className="m-0 mb-2 list-disc pl-5 last:mb-0">{children}</ul>,
  ol: ({ children }) => <ol className="m-0 mb-2 list-decimal pl-5 last:mb-0">{children}</ol>,
  li: ({ children }) => <li className="mb-1 last:mb-0">{children}</li>,
  strong: ({ children }) => <strong className="font-semibold text-[#e6e6e6]">{children}</strong>,
  em: ({ children }) => <em className="italic">{children}</em>,
  del: ({ children }) => <del className="opacity-70">{children}</del>,
  a: ({ children, href }) => {
    const citationId = parseCitationHref(href);
    if (citationId !== null) {
      return <CitationChip citationId={citationId}>{children}</CitationChip>;
    }
    if (isInternalHref(href)) {
      return (
        <Link className={LINK_CLASS} to={href}>
          {children}
        </Link>
      );
    }
    return (
      <a className={LINK_CLASS} href={href} rel="noreferrer" target="_blank">
        {children}
      </a>
    );
  },
  code: ({ children }) => (
    <code className="border border-[#333] bg-[#252525] px-1 py-0.5 font-mono text-[12px] text-[#d4d4d4]">{children}</code>
  ),
  pre: ({ children }) => (
    <pre className="m-0 mb-2 overflow-x-auto border border-[#333] bg-[#252525] p-3 text-[12px] text-[#d4d4d4] last:mb-0 [&>code]:border-0 [&>code]:bg-transparent [&>code]:p-0">
      {children}
    </pre>
  ),
  blockquote: ({ children }) => (
    <blockquote className="m-0 mb-2 border-l-2 border-[#333] pl-3 text-[#9aa0a6] last:mb-0">{children}</blockquote>
  ),
  h1: ({ children }) => <h1 className="mt-3 mb-2 text-base font-semibold text-[#e6e6e6] first:mt-0">{children}</h1>,
  h2: ({ children }) => <h2 className="mt-3 mb-2 text-base font-semibold text-[#e6e6e6] first:mt-0">{children}</h2>,
  h3: ({ children }) => <h3 className="mt-2 mb-1 text-sm font-semibold text-[#e6e6e6] first:mt-0">{children}</h3>,
  h4: ({ children }) => <h4 className="mt-2 mb-1 text-sm font-semibold text-[#e6e6e6] first:mt-0">{children}</h4>,
  h5: ({ children }) => <h5 className="mt-2 mb-1 text-sm font-semibold text-[#e6e6e6] first:mt-0">{children}</h5>,
  h6: ({ children }) => <h6 className="mt-2 mb-1 text-sm font-semibold text-[#e6e6e6] first:mt-0">{children}</h6>,
  hr: () => <hr className="my-3 border-[#333]" />,
  table: ({ children }) => (
    <div className="mb-2 overflow-x-auto last:mb-0">
      <table className="border-collapse border border-[#333] text-xs">{children}</table>
    </div>
  ),
  thead: ({ children }) => <thead className="bg-[#252525]">{children}</thead>,
  th: ({ children }) => <th className="border border-[#333] px-2 py-1 text-left font-semibold text-[#e6e6e6]">{children}</th>,
  td: ({ children }) => <td className="border border-[#333] px-2 py-1 align-top">{children}</td>,
};

export function MarkdownContent({ text }: { text: string }) {
  return (
    <div className="markdown-content">
      <Markdown components={COMPONENTS} remarkPlugins={REMARK_PLUGINS}>
        {preprocessCitations(text)}
      </Markdown>
    </div>
  );
}
