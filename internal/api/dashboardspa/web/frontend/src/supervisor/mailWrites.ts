import { activeCityOrThrow } from '../api/cityBase';
import { supervisorApi } from './client';

export interface MailComposeDraft {
  to: string;
  subject: string;
  body: string;
}

export interface MailReplyDraft {
  body: string;
}

export interface MailActionTarget {
  id: string;
  rig?: string;
}

export async function sendSupervisorMail(
  draft: MailComposeDraft,
  operatorWireAlias: string,
): Promise<void> {
  await supervisorApi().sendMail(activeCityOrThrow('send supervisor mail'), {
    ...draft,
    from: operatorWireAlias,
  });
}

export async function markSupervisorMailRead(message: MailActionTarget): Promise<void> {
  await supervisorApi().markMailRead(
    activeCityOrThrow('mark supervisor mail read'),
    message.id,
    mailActionQuery(message),
  );
}

export async function markSupervisorMailUnread(message: MailActionTarget): Promise<void> {
  await supervisorApi().markMailUnread(
    activeCityOrThrow('mark supervisor mail unread'),
    message.id,
    mailActionQuery(message),
  );
}

export async function archiveSupervisorMail(message: MailActionTarget): Promise<void> {
  await supervisorApi().archiveMail(
    activeCityOrThrow('archive supervisor mail'),
    message.id,
    mailActionQuery(message),
  );
}

export async function replySupervisorMail(
  message: MailActionTarget,
  draft: MailReplyDraft,
  operatorWireAlias: string,
): Promise<void> {
  await supervisorApi().replyMail(
    activeCityOrThrow('reply supervisor mail'),
    message.id,
    {
      ...draft,
      from: operatorWireAlias,
    },
    mailActionQuery(message),
  );
}

function mailActionQuery(message: MailActionTarget): { rig: string } | undefined {
  return message.rig === undefined || message.rig.length === 0 ? undefined : { rig: message.rig };
}
