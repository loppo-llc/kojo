import { useMemo } from "react";
import { useNavigate } from "react-router";
import { api } from "../lib/api";
import { FileDataBrowser } from "./FileDataBrowser";
import { useT } from "../lib/i18n";

interface FileBrowserProps {
  embedded?: boolean;
  initialPath?: string;
  // peerId routes file API calls through the Hub→peer proxy so the
  // browser shows the remote host's filesystem when a peer-routed
  // session owns this view.
  peerId?: string;
}

export function FileBrowser({ embedded, initialPath, peerId }: FileBrowserProps = {}) {
  const t = useT();
  const navigate = useNavigate();
  const dataSource = useMemo(() => ({
    list: (path: string, hidden: boolean) =>
      api.files.list(path || undefined, hidden, peerId).then((result) => ({
        path: result.path,
        absPath: result.path,
        entries: result.entries,
      })),
    view: (path: string) => api.files.view(path, peerId),
    save: (path: string, content: string, etag: string) => api.files.save(path, content, etag, peerId),
    rawUrl: (path: string, download?: boolean) => api.files.rawUrl(path, download, peerId),
    thumbUrl: (path: string, size?: number, v?: string) => api.files.thumbUrl(path, size, v, peerId),
  }), [peerId]);

  return (
    <FileDataBrowser
      dataSource={dataSource}
      pathMode="absolute"
      pathParam="path"
      rootPath={embedded ? initialPath : undefined}
      rootLabel={embedded ? t("fb.workdir") : t("fb.files")}
      title={t("fb.files")}
      subtitle={embedded ? undefined : t("fb.localFs")}
      showHeader={!embedded}
      ready={!embedded || Boolean(initialPath)}
      onExit={() => navigate("/")}
    />
  );
}
