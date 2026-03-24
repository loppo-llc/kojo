import { Children, isValidElement, cloneElement, createContext, useContext, useMemo } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

const InsideLinkCtx = createContext(false);
const InsidePreCtx = createContext(false);

interface MarkdownRendererProps {
  content: string;
  /** Optional callback to transform plain-text segments (e.g. file-path chips). */
  processText?: (text: string) => React.ReactNode;
}

/** Skip text processing inside these intrinsic element types. */
const SKIP_TAGS = new Set(["code", "pre", "a"]);

/**
 * Recursively apply processText to string children.
 * Skips code/pre/a intrinsic elements and all custom (function) components.
 * Only recurses into intrinsic HTML elements like strong, em, span, etc.
 */
function mapTextChildren(
  children: React.ReactNode,
  processText: (text: string) => React.ReactNode,
): React.ReactNode {
  return Children.map(children, (child) => {
    if (typeof child === "string") return processText(child);
    if (isValidElement(child)) {
      // Only recurse into intrinsic HTML elements (string type), not custom components
      if (typeof child.type !== "string") return child;
      if (SKIP_TAGS.has(child.type)) return child;
      const props = child.props as { children?: React.ReactNode };
      if (props.children != null) {
        return cloneElement(child, {}, mapTextChildren(props.children, processText));
      }
    }
    return child;
  });
}

export function MarkdownRenderer({ content, processText }: MarkdownRendererProps) {
  const components = useMemo(
    () => {
      const withText = (Tag: string) => {
        if (!processText) return undefined;
        return ({ children, node: _node, ...props }: any) => {
          const El = Tag as any;
          return <El {...props}>{mapTextChildren(children, processText)}</El>;
        };
      };

      return {
        pre({ children, ...props }: React.ComponentProps<"pre">) {
          // Extract language from code child's className if present
          let lang = "";
          const child = Array.isArray(children) ? children[0] : children;
          if (child && typeof child === "object" && "props" in child) {
            const cls =
              (child as React.ReactElement<{ className?: string }>).props
                .className || "";
            lang = cls.replace("language-", "");
          }
          return (
            <InsidePreCtx.Provider value={true}>
              <div className="md-code-wrap">
                {lang && <div className="md-code-lang">{lang}</div>}
                <pre {...props}>{children}</pre>
              </div>
            </InsidePreCtx.Provider>
          );
        },
        a({ children, href, ...props }: React.ComponentProps<"a">) {
          return (
            <InsideLinkCtx.Provider value={true}>
              <a
                href={href}
                target="_blank"
                rel="noopener noreferrer"
                className="md-link"
                {...props}
              >
                {children}
              </a>
            </InsideLinkCtx.Provider>
          );
        },
        ...(processText && {
          p: withText("p"),
          li: withText("li"),
          td: withText("td"),
          th: withText("th"),
          h1: withText("h1"),
          h2: withText("h2"),
          h3: withText("h3"),
          h4: withText("h4"),
          h5: withText("h5"),
          h6: withText("h6"),
          blockquote: withText("blockquote"),
          code({ children, node: _node, className, ...props }: any) {
            const insideLink = useContext(InsideLinkCtx);
            const insidePre = useContext(InsidePreCtx);
            // Only process inline code (skip code blocks and code inside links).
            // Replace only when the entire content is a single file path.
            const text = typeof children === "string" ? children
              : Array.isArray(children) && children.length === 1 && typeof children[0] === "string" ? children[0]
              : null;
            if (!insidePre && !insideLink && text) {
              const result = processText(text);
              if (Array.isArray(result) && result.length === 1 && typeof result[0] !== "string") {
                return <>{result}</>;
              }
            }
            return <code className={className} {...props}>{children}</code>;
          },
        }),
      };
    },
    [processText],
  );

  return (
    <div className="md-content">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>
        {content}
      </ReactMarkdown>
    </div>
  );
}
