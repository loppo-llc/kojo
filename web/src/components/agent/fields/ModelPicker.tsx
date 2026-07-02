import { effortLevelsForModel, type EffortLevel } from "../../../lib/toolModels";
import { Field } from "../../ui/Field";
import { Select } from "../../ui/Select";
import { Input } from "../../ui/Input";

/**
 * The "Model" field: a <select> when the backend has a known model list,
 * otherwise a free-text <input>. Changing the model resets effort when the
 * new model no longer supports the current effort level. `models` is the
 * caller-resolved list (backend whitelist or fetched custom models).
 */
export function ModelPicker({
  model,
  setModel,
  effort,
  setEffort,
  models,
}: {
  model: string;
  setModel: (m: string) => void;
  effort: EffortLevel | "";
  setEffort: (e: EffortLevel | "") => void;
  models: string[];
}) {
  const onChange = (m: string) => {
    setModel(m);
    if (effort && !effortLevelsForModel(m).includes(effort)) setEffort("");
  };
  return (
    <Field label="Model">
      {models.length > 0 ? (
        <Select value={model} onChange={(e) => onChange(e.target.value)}>
          {model && !models.includes(model) && (
            <option value={model}>{model}</option>
          )}
          {models.map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </Select>
      ) : (
        <Input
          mono
          value={model}
          onChange={(e) => onChange(e.target.value)}
          placeholder="model name"
        />
      )}
    </Field>
  );
}
