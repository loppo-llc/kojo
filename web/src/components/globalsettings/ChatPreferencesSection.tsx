import { useEffect, useState } from "react";
import { SectionCard } from "../ui/SectionCard";
import { Toggle } from "../ui/Toggle";
import { Field } from "../ui/Field";
import { Select } from "../ui/Select";
import { useT } from "../../lib/i18n";
import { agentApi, type STTProvider } from "../../lib/agentApi";
import { refreshSTTConfig, setSTTConfig, useSTTConfig } from "../../lib/sttConfig";
import { errMsg } from "../../lib/utils";

interface Props {
  enterSends: boolean;
  setEnterSends: (v: boolean) => void;
  /**
   * Changes whenever an STT-capable API key (xAI / Gemini) is saved or
   * removed on this page, so provider availability is re-fetched.
   */
  sttKeyRevision: string;
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

const PROVIDER_NAMES: Record<Exclude<STTProvider, "off">, string> = {
  gemini: "Gemini API (gemini-3.5-transcribe-live)",
  xai: "xAI API",
};

/** Chat preferences section — Enter key send behavior and voice input provider. */
export function ChatPreferencesSection({ enterSends, setEnterSends, sttKeyRevision, setError, flashSuccess }: Props) {
  const t = useT();
  const stt = useSTTConfig();
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    void refreshSTTConfig();
  }, [sttKeyRevision]);

  const changeProvider = async (next: STTProvider) => {
    if (!stt || next === stt.provider) return;
    setSaving(true);
    setError("");
    try {
      setSTTConfig(await agentApi.stt.setConfig(next));
      flashSuccess();
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <SectionCard title={t("gs.chat")}>
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-[13px] text-ink">{t("gs.sendWithEnter")}</div>
          <div className="mt-0.5 text-[12px] text-ink-faint">
            {enterSends ? t("gs.enterSendsHelp") : t("gs.ctrlEnterSendsHelp")}
          </div>
        </div>
        <Toggle checked={enterSends} onChange={setEnterSends} aria-label={t("gs.sendWithEnter")} />
      </div>
      <Field className="mt-4" label={t("gs.voiceInput")} htmlFor="gs-voice-input" help={t("gs.voiceInputHelp")}>
        <Select
          id="gs-voice-input"
          value={stt?.provider ?? "off"}
          disabled={!stt || saving}
          onChange={(e) => void changeProvider(e.target.value as STTProvider)}
        >
          <option value="off">{t("gs.voiceInputOff")}</option>
          {(["gemini", "xai"] as const).map((p) => {
            const available = stt?.available[p] ?? false;
            return (
              <option key={p} value={p} disabled={!available}>
                {available ? PROVIDER_NAMES[p] : t("gs.voiceInputNoKey", { name: PROVIDER_NAMES[p] })}
              </option>
            );
          })}
        </Select>
      </Field>
    </SectionCard>
  );
}
