import { useEffect, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router";
import { FileDataBrowser } from "../FileDataBrowser";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { AgentAvatar } from "./AgentAvatar";

export function AgentDataBrowser() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [agent, setAgent] = useState<AgentInfo | null>(null);

  useEffect(() => {
    if (!id) return;
    let cancelled = false;
    setAgent(null);
    agentApi
      .get(id)
      .then((a) => {
        if (!cancelled) setAgent(a);
      })
      .catch(() => {
        if (!cancelled) navigate("/");
      });
    return () => {
      cancelled = true;
    };
  }, [id, navigate]);

  const dataSource = useMemo(() => {
    if (!id) return null;
    return {
      list: (path: string, hidden: boolean) => agentApi.files.list(id, path, hidden),
      view: (path: string) => agentApi.files.view(id, path),
      rawUrl: (path: string, download?: boolean) => agentApi.files.rawUrl(id, path, download),
      thumbUrl: (path: string, size?: number, v?: string) => agentApi.files.thumbUrl(id, path, size, v),
    };
  }, [id]);

  if (!id || !dataSource) return null;

  return (
    <FileDataBrowser
      dataSource={dataSource}
      pathMode="relative"
      pathParam="sub"
      rootLabel="Data"
      title={agent?.name ?? " "}
      subtitle="Data folder"
      leading={
        agent ? (
          <AgentAvatar agentId={agent.id} name={agent.name} size="sm" cacheBust={agent.avatarHash} />
        ) : (
          <div className="w-8 h-8 rounded-full bg-neutral-800" />
        )
      }
      onExit={() => navigate(`/agents/${id}`, { replace: true })}
    />
  );
}
